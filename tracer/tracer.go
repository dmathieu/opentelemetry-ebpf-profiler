// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package tracer contains functionality for populating tracers.
package tracer // import "go.opentelemetry.io/ebpf-profiler/tracer"

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/elastic/go-perf"
	log "github.com/sirupsen/logrus"
	"github.com/zeebo/xxh3"

	"go.opentelemetry.io/ebpf-profiler/host"
	"go.opentelemetry.io/ebpf-profiler/kallsyms"
	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/libpf/xsync"
	"go.opentelemetry.io/ebpf-profiler/metrics"
	"go.opentelemetry.io/ebpf-profiler/nativeunwind/elfunwindinfo"
	"go.opentelemetry.io/ebpf-profiler/periodiccaller"
	pm "go.opentelemetry.io/ebpf-profiler/processmanager"
	pmebpf "go.opentelemetry.io/ebpf-profiler/processmanager/ebpf"
	"go.opentelemetry.io/ebpf-profiler/reporter"
	"go.opentelemetry.io/ebpf-profiler/rlimit"
	"go.opentelemetry.io/ebpf-profiler/support"
	"go.opentelemetry.io/ebpf-profiler/times"
	"go.opentelemetry.io/ebpf-profiler/tracehandler"
	"go.opentelemetry.io/ebpf-profiler/tracer/types"
)

// Compile time check to make sure config.Times satisfies the interfaces.
var _ Intervals = (*times.Times)(nil)

const (
	// ProbabilisticThresholdMax defines the upper bound of the probabilistic profiling
	// threshold.
	ProbabilisticThresholdMax = 100
)

// Constants that define the status of probabilistic profiling.
const (
	probProfilingEnable  = 1
	probProfilingDisable = -1
)

// Intervals is a subset of config.IntervalsAndTimers.
type Intervals interface {
	MonitorInterval() time.Duration
	TracePollInterval() time.Duration
	PIDCleanupInterval() time.Duration
}

// Tracer provides an interface for loading and initializing the eBPF components as
// well as for monitoring the output maps for new traces and count updates.
type Tracer struct {
	fallbackSymbolHit  atomic.Uint64
	fallbackSymbolMiss atomic.Uint64

	// ebpfMaps holds the currently loaded eBPF maps.
	ebpfMaps map[string]*cebpf.Map
	// ebpfProgs holds the currently loaded eBPF programs.
	ebpfProgs map[string]*cebpf.Program

	// kernelSymbolizer does kernel fallback symbolization
	kernelSymbolizer *kallsyms.Symbolizer

	// perfEntrypoints holds a list of frequency based perf events that are opened on the system.
	perfEntrypoints xsync.RWMutex[[]*perf.Event]

	// hooks holds references to loaded eBPF hooks.
	hooks map[hookPoint]link.Link

	// processManager keeps track of loading, unloading and organization of information
	// that is required to unwind processes in the kernel. This includes maintaining the
	// associated eBPF maps.
	processManager *pm.ProcessManager

	// triggerPIDProcessing is used as manual trigger channel to request immediate
	// processing of pending PIDs. This is requested on notifications from eBPF code
	// when process events take place (new, exit, unknown PC).
	triggerPIDProcessing chan bool

	// pidEvents notifies the tracer of new PID events. Each PID event is a 64bit integer
	// value, see bpf_get_current_pid_tgid for information on how the value is encoded.
	// It needs to be buffered to avoid locking the writers and stacking up resources when we
	// read new PIDs at startup or notified via eBPF.
	pidEvents chan libpf.PIDTID

	// intervals provides access to globally configured timers and counters.
	intervals Intervals

	hasBatchOperations bool

	// reporter allows swapping out the reporter implementation.
	reporter reporter.SymbolReporter

	// samplesPerSecond holds the configured number of samples per second.
	samplesPerSecond int

	// probabilisticInterval is the time interval for which probabilistic profiling will be enabled.
	probabilisticInterval time.Duration

	// probabilisticThreshold holds the threshold for probabilistic profiling.
	probabilisticThreshold uint
}

type Config struct {
	// Reporter allows swapping out the reporter implementation.
	Reporter reporter.SymbolReporter
	// Intervals provides access to globally configured timers and counters.
	Intervals Intervals
	// IncludeTracers holds information about which tracers are enabled.
	IncludeTracers types.IncludedTracers
	// SamplesPerSecond holds the number of samples per second.
	SamplesPerSecond int
	// MapScaleFactor is the scaling factor for eBPF map sizes.
	MapScaleFactor int
	// FilterErrorFrames indicates whether error frames should be filtered.
	FilterErrorFrames bool
	// KernelVersionCheck indicates whether the kernel version should be checked.
	KernelVersionCheck bool
	// DebugTracer indicates whether to load the debug version of eBPF tracers.
	DebugTracer bool
	// BPFVerifierLogLevel is the log level of the eBPF verifier output.
	BPFVerifierLogLevel uint32
	// ProbabilisticInterval is the time interval for which probabilistic profiling will be enabled.
	ProbabilisticInterval time.Duration
	// ProbabilisticThreshold is the threshold for probabilistic profiling.
	ProbabilisticThreshold uint
	// OffCPUThreshold is the user defined threshold for off-cpu profiling.
	OffCPUThreshold uint32
	// IncludeEnvVars holds a list of environment variables that should be captured and reported
	// from processes
	IncludeEnvVars libpf.Set[string]
}

// hookPoint specifies the group and name of the hooked point in the kernel.
type hookPoint struct {
	group, name string
}

// progLoaderHelper supports the loading process of eBPF programs.
type progLoaderHelper struct {
	// enable tells whether a prog shall be loaded.
	enable bool
	// name of the eBPF program
	name string
	// progID defines the ID for the eBPF program that is used as key in the tailcallMap.
	progID uint32
	// noTailCallTarget indicates if this eBPF program should be added to the tailcallMap.
	noTailCallTarget bool
}

// Convert a C-string to Go string.
func goString(cstr []byte) string {
	index := bytes.IndexByte(cstr, byte(0))
	if index < 0 {
		index = len(cstr)
	}
	return strings.Clone(unsafe.String(unsafe.SliceData(cstr), index))
}

// NewTracer loads eBPF code and map definitions from the ELF module at the configured path.
func NewTracer(ctx context.Context, cfg *Config) (*Tracer, error) {
	kernelSymbolizer, err := kallsyms.NewSymbolizer()
	if err != nil {
		return nil, fmt.Errorf("failed to read kernel symbols: %v", err)
	}

	kmod, err := kernelSymbolizer.GetModuleByName(kallsyms.Kernel)
	if err != nil {
		return nil, fmt.Errorf("failed to read kernel symbols: %v", err)
	}

	// Based on includeTracers we decide later which are loaded into the kernel.
	ebpfMaps, ebpfProgs, err := initializeMapsAndPrograms(kmod, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to load eBPF code: %v", err)
	}

	ebpfHandler, err := pmebpf.LoadMaps(ctx, ebpfMaps)
	if err != nil {
		return nil, fmt.Errorf("failed to load eBPF maps: %v", err)
	}

	hasBatchOperations := ebpfHandler.SupportsGenericBatchOperations()

	processManager, err := pm.New(ctx, cfg.IncludeTracers, cfg.Intervals.MonitorInterval(),
		ebpfHandler, nil, cfg.Reporter, elfunwindinfo.NewStackDeltaProvider(),
		cfg.FilterErrorFrames, cfg.IncludeEnvVars)
	if err != nil {
		return nil, fmt.Errorf("failed to create processManager: %v", err)
	}

	perfEventList := []*perf.Event{}

	tracer := &Tracer{
		kernelSymbolizer:       kernelSymbolizer,
		processManager:         processManager,
		triggerPIDProcessing:   make(chan bool, 1),
		pidEvents:              make(chan libpf.PIDTID, pidEventBufferSize),
		ebpfMaps:               ebpfMaps,
		ebpfProgs:              ebpfProgs,
		hooks:                  make(map[hookPoint]link.Link),
		intervals:              cfg.Intervals,
		hasBatchOperations:     hasBatchOperations,
		perfEntrypoints:        xsync.NewRWMutex(perfEventList),
		reporter:               cfg.Reporter,
		samplesPerSecond:       cfg.SamplesPerSecond,
		probabilisticInterval:  cfg.ProbabilisticInterval,
		probabilisticThreshold: cfg.ProbabilisticThreshold,
	}

	return tracer, nil
}

// Close provides functionality for Tracer to perform cleanup tasks.
// NOTE: Close may be called multiple times in succession.
func (t *Tracer) Close() {
	events := t.perfEntrypoints.WLock()
	for _, event := range *events {
		if err := event.Disable(); err != nil {
			log.Errorf("Failed to disable perf event: %v", err)
		}
		if err := event.Close(); err != nil {
			log.Errorf("Failed to close perf event: %v", err)
		}
	}
	*events = nil
	t.perfEntrypoints.WUnlock(&events)

	// Avoid resource leakage by closing all kernel hooks.
	for hookPoint, hook := range t.hooks {
		if err := hook.Close(); err != nil {
			log.Errorf("Failed to close '%s/%s': %v", hookPoint.group, hookPoint.name, err)
		}
		delete(t.hooks, hookPoint)
	}

	t.processManager.Close()
}

func buildStackDeltaTemplates(coll *cebpf.CollectionSpec) error {
	// Prepare the inner map template of the stack deltas map-of-maps.
	// This cannot be provided from the eBPF C code, and needs to be done here.
	for i := support.StackDeltaBucketSmallest; i <= support.StackDeltaBucketLargest; i++ {
		mapName := fmt.Sprintf("exe_id_to_%d_stack_deltas", i)
		def := coll.Maps[mapName]
		if def == nil {
			return fmt.Errorf("ebpf map '%s' not found", mapName)
		}
		def.InnerMap = &cebpf.MapSpec{
			Type:       cebpf.Array,
			KeySize:    4,
			ValueSize:  support.Sizeof_StackDelta,
			MaxEntries: 1 << i,
		}
	}
	return nil
}

// initializeMapsAndPrograms loads the definitions for the eBPF maps and programs provided
// by the embedded elf file and loads these into the kernel.
func initializeMapsAndPrograms(kmod *kallsyms.Module, cfg *Config) (
	ebpfMaps map[string]*cebpf.Map, ebpfProgs map[string]*cebpf.Program, err error) {
	// Loading specifications about eBPF programs and maps from the embedded elf file
	// does not load them into the kernel.
	// A collection specification holds the information about eBPF programs and maps.
	// References to eBPF maps in the eBPF programs are just placeholders that need to be
	// replaced by the actual loaded maps later on with RewriteMaps before loading the
	// programs into the kernel.
	coll, err := support.LoadCollectionSpec()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load specification for tracers: %v", err)
	}

	if cfg.DebugTracer {
		if err = coll.Variables["with_debug_output"].Set(uint32(1)); err != nil {
			return nil, nil, fmt.Errorf("failed to set debug output: %v", err)
		}
	}

	err = buildStackDeltaTemplates(coll)
	if err != nil {
		return nil, nil, err
	}

	ebpfMaps = make(map[string]*cebpf.Map)
	ebpfProgs = make(map[string]*cebpf.Program)

	// Load all maps into the kernel that are used later on in eBPF programs. So we can rewrite
	// in the next step the placesholders in the eBPF programs with the file descriptors of the
	// loaded maps in the kernel.
	if err = loadAllMaps(coll, cfg, ebpfMaps); err != nil {
		return nil, nil, fmt.Errorf("failed to load eBPF maps: %v", err)
	}

	// Replace the place holders for map access in the eBPF programs with
	// the file descriptors of the loaded maps.
	//nolint:staticcheck
	if err = coll.RewriteMaps(ebpfMaps); err != nil {
		return nil, nil, fmt.Errorf("failed to rewrite maps: %v", err)
	}

	if cfg.KernelVersionCheck {
		var major, minor, patch uint32
		major, minor, patch, err = GetCurrentKernelVersion()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get kernel version: %v", err)
		}
		if hasProbeReadBug(major, minor, patch) {
			if err = checkForMaccessPatch(coll, ebpfMaps, kmod); err != nil {
				return nil, nil, fmt.Errorf("your kernel version %d.%d.%d may be "+
					"affected by a Linux kernel bug that can lead to system "+
					"freezes, terminating host agent now to avoid "+
					"triggering this bug.\n"+
					"If you are certain your kernel is not affected, "+
					"you can override this check at your own risk "+
					"with -no-kernel-version-check.\n"+
					"Error: %v", major, minor, patch, err)
			}
		}
	}

	tailCallProgs := []progLoaderHelper{
		{
			progID: uint32(support.ProgUnwindStop),
			name:   "unwind_stop",
			enable: true,
		},
		{
			progID: uint32(support.ProgUnwindNative),
			name:   "unwind_native",
			enable: true,
		},
		{
			progID: uint32(support.ProgUnwindHotspot),
			name:   "unwind_hotspot",
			enable: cfg.IncludeTracers.Has(types.HotspotTracer),
		},
		{
			progID: uint32(support.ProgUnwindPerl),
			name:   "unwind_perl",
			enable: cfg.IncludeTracers.Has(types.PerlTracer),
		},
		{
			progID: uint32(support.ProgUnwindPHP),
			name:   "unwind_php",
			enable: cfg.IncludeTracers.Has(types.PHPTracer),
		},
		{
			progID: uint32(support.ProgUnwindPython),
			name:   "unwind_python",
			enable: cfg.IncludeTracers.Has(types.PythonTracer),
		},
		{
			progID: uint32(support.ProgUnwindRuby),
			name:   "unwind_ruby",
			enable: cfg.IncludeTracers.Has(types.RubyTracer),
		},
		{
			progID: uint32(support.ProgUnwindV8),
			name:   "unwind_v8",
			enable: cfg.IncludeTracers.Has(types.V8Tracer),
		},
		{
			progID: uint32(support.ProgUnwindDotnet),
			name:   "unwind_dotnet",
			enable: cfg.IncludeTracers.Has(types.DotnetTracer),
		},
		{
			progID: uint32(support.ProgGoLabels),
			name:   "go_labels",
			enable: cfg.IncludeTracers.Has(types.Labels),
		},
	}

	if err = loadPerfUnwinders(coll, ebpfProgs, ebpfMaps["perf_progs"], tailCallProgs,
		cfg.BPFVerifierLogLevel); err != nil {
		return nil, nil, fmt.Errorf("failed to load perf eBPF programs: %v", err)
	}

	if cfg.OffCPUThreshold > 0 {
		if err = loadKProbeUnwinders(coll, ebpfProgs, ebpfMaps["kprobe_progs"], tailCallProgs,
			cfg.BPFVerifierLogLevel, ebpfMaps["perf_progs"].FD()); err != nil {
			return nil, nil, fmt.Errorf("failed to load kprobe eBPF programs: %v", err)
		}
	}

	if err = loadSystemConfig(coll, ebpfMaps, kmod, cfg.IncludeTracers,
		cfg.OffCPUThreshold, cfg.FilterErrorFrames); err != nil {
		return nil, nil, fmt.Errorf("failed to load system config: %v", err)
	}

	if err = removeTemporaryMaps(ebpfMaps); err != nil {
		return nil, nil, fmt.Errorf("failed to remove temporary maps: %v", err)
	}

	return ebpfMaps, ebpfProgs, nil
}

// removeTemporaryMaps unloads and deletes eBPF maps that are only required for the
// initialization.
func removeTemporaryMaps(ebpfMaps map[string]*cebpf.Map) error {
	for _, mapName := range []string{"system_analysis"} {
		if err := ebpfMaps[mapName].Close(); err != nil {
			log.Errorf("Failed to close %s: %v", mapName, err)
			return err
		}
		delete(ebpfMaps, mapName)
	}
	return nil
}

// loadAllMaps loads all eBPF maps that are used in our eBPF programs.
func loadAllMaps(coll *cebpf.CollectionSpec, cfg *Config,
	ebpfMaps map[string]*cebpf.Map) error {
	restoreRlimit, err := rlimit.MaximizeMemlock()
	if err != nil {
		return fmt.Errorf("failed to adjust rlimit: %v", err)
	}
	defer restoreRlimit()

	// Redefine the maximum number of map entries for selected eBPF maps.
	adaption := make(map[string]uint32, 4)

	const (
		// The following sizes X are used as 2^X, and determined empirically.
		// 1 million executable pages / 4GB of executable address space
		pidPageMappingInfoSize   = 20
		stackDeltaPageToInfoSize = 16
		exeIDToStackDeltasSize   = 16
	)

	adaption["pid_page_to_mapping_info"] =
		1 << uint32(pidPageMappingInfoSize+cfg.MapScaleFactor)

	adaption["stack_delta_page_to_info"] =
		1 << uint32(stackDeltaPageToInfoSize+cfg.MapScaleFactor)

	adaption["sched_times"] = schedTimesSize(cfg.OffCPUThreshold)

	for i := support.StackDeltaBucketSmallest; i <= support.StackDeltaBucketLargest; i++ {
		mapName := fmt.Sprintf("exe_id_to_%d_stack_deltas", i)
		adaption[mapName] = 1 << uint32(exeIDToStackDeltasSize+cfg.MapScaleFactor)
	}

	for mapName, mapSpec := range coll.Maps {
		if mapName == "sched_times" && cfg.OffCPUThreshold == 0 {
			// Off CPU Profiling is disabled. So do not load this map.
			continue
		}
		if newSize, ok := adaption[mapName]; ok {
			log.Debugf("Size of eBPF map %s: %v", mapName, newSize)
			mapSpec.MaxEntries = newSize
		}
		ebpfMap, err := cebpf.NewMap(mapSpec)
		if err != nil {
			return fmt.Errorf("failed to load %s: %v", mapName, err)
		}
		ebpfMaps[mapName] = ebpfMap
	}

	return nil
}

// schedTimesSize calculates the size of the sched_times map based on the
// configured off-cpu threshold.
// To not lose too many scheduling events but also not oversize sched_times,
// calculate a size based on an assumed upper bound of scheduler events per
// second (1000hz) multiplied by an average time a task remains off CPU (3s),
// scaled by the probability of capturing a trace.
func schedTimesSize(threshold uint32) uint32 {
	size := uint32((4096 * uint64(threshold)) / math.MaxUint32)
	if size < 16 {
		// Guarantee a minimal size of 16.
		return 16
	}
	if size > 4096 {
		// Guarantee a maximum size of 4096.
		return 4096
	}
	return size
}

// loadPerfUnwinders loads all perf eBPF Programs and their tail call targets.
func loadPerfUnwinders(coll *cebpf.CollectionSpec, ebpfProgs map[string]*cebpf.Program,
	tailcallMap *cebpf.Map, tailCallProgs []progLoaderHelper,
	bpfVerifierLogLevel uint32) error {
	programOptions := cebpf.ProgramOptions{
		LogLevel: cebpf.LogLevel(bpfVerifierLogLevel),
	}

	progs := make([]progLoaderHelper, len(tailCallProgs)+2)
	copy(progs, tailCallProgs)
	progs = append(progs,
		progLoaderHelper{
			name:             "tracepoint__sched_process_free",
			noTailCallTarget: true,
			enable:           true,
		},
		progLoaderHelper{
			name:             "native_tracer_entry",
			noTailCallTarget: true,
			enable:           true,
		})

	for _, unwindProg := range progs {
		if !unwindProg.enable {
			continue
		}

		unwindProgName := unwindProg.name
		if !unwindProg.noTailCallTarget {
			unwindProgName = "perf_" + unwindProg.name
		}

		progSpec, ok := coll.Programs[unwindProgName]
		if !ok {
			return fmt.Errorf("program %s does not exist", unwindProgName)
		}

		if err := loadProgram(ebpfProgs, tailcallMap, unwindProg.progID, progSpec,
			programOptions, unwindProg.noTailCallTarget); err != nil {
			return err
		}
	}

	return nil
}

// progArrayReferences returns a list of instructions which load a specified tail
// call FD.
func progArrayReferences(perfTailCallMapFD int, insns asm.Instructions) []int {
	insNos := []int{}
	for i := range insns {
		ins := &insns[i]
		if asm.OpCode(ins.OpCode.Class()) != asm.OpCode(asm.LdClass) {
			continue
		}
		m := ins.Map()
		if m == nil {
			continue
		}
		if perfTailCallMapFD == m.FD() {
			insNos = append(insNos, i)
		}
	}
	return insNos
}

// loadKProbeUnwinders reuses large parts of loadPerfUnwinders. By default all eBPF programs
// are written as perf event eBPF programs. loadKProbeUnwinders dynamically rewrites the
// specification of these programs to kprobe eBPF programs and adjusts tail call maps.
func loadKProbeUnwinders(coll *cebpf.CollectionSpec, ebpfProgs map[string]*cebpf.Program,
	tailcallMap *cebpf.Map, tailCallProgs []progLoaderHelper,
	bpfVerifierLogLevel uint32, perfTailCallMapFD int) error {
	programOptions := cebpf.ProgramOptions{
		LogLevel: cebpf.LogLevel(bpfVerifierLogLevel),
	}

	progs := make([]progLoaderHelper, len(tailCallProgs)+2)
	copy(progs, tailCallProgs)
	progs = append(progs,
		progLoaderHelper{
			name:             "finish_task_switch",
			noTailCallTarget: true,
			enable:           true,
		},
		progLoaderHelper{
			name:             "tracepoint__sched_switch",
			noTailCallTarget: true,
			enable:           true,
		},
	)

	for _, unwindProg := range progs {
		if !unwindProg.enable {
			continue
		}

		unwindProgName := unwindProg.name
		if !unwindProg.noTailCallTarget {
			unwindProgName = "kprobe_" + unwindProg.name
		}

		progSpec, ok := coll.Programs[unwindProgName]
		if !ok {
			return fmt.Errorf("program %s does not exist", unwindProgName)
		}

		// Replace the prog array for the tail calls.
		insns := progArrayReferences(perfTailCallMapFD, progSpec.Instructions)
		for _, ins := range insns {
			if err := progSpec.Instructions[ins].AssociateMap(tailcallMap); err != nil {
				return fmt.Errorf("failed to rewrite map ptr: %v", err)
			}
		}

		if err := loadProgram(ebpfProgs, tailcallMap, unwindProg.progID, progSpec,
			programOptions, unwindProg.noTailCallTarget); err != nil {
			return err
		}
	}

	return nil
}

// loadProgram loads an eBPF program from progSpec and populates the related maps.
func loadProgram(ebpfProgs map[string]*cebpf.Program, tailcallMap *cebpf.Map,
	progID uint32, progSpec *cebpf.ProgramSpec, programOptions cebpf.ProgramOptions,
	noTailCallTarget bool) error {
	restoreRlimit, err := rlimit.MaximizeMemlock()
	if err != nil {
		return fmt.Errorf("failed to adjust rlimit: %v", err)
	}
	defer restoreRlimit()

	// Load the eBPF program into the kernel. If no error is returned,
	// the eBPF program can be used/called/triggered from now on.
	unwinder, err := cebpf.NewProgramWithOptions(progSpec, programOptions)
	if err != nil {
		// These errors tend to have hundreds of lines (or more),
		// so we print each line individually.
		if ve, ok := err.(*cebpf.VerifierError); ok {
			for _, line := range ve.Log {
				log.Error(line)
			}
		} else {
			scanner := bufio.NewScanner(strings.NewReader(err.Error()))
			for scanner.Scan() {
				log.Error(scanner.Text())
			}
		}
		return fmt.Errorf("failed to load %s", progSpec.Name)
	}
	ebpfProgs[progSpec.Name] = unwinder

	if noTailCallTarget {
		return nil
	}
	fd := uint32(unwinder.FD())
	if err := tailcallMap.Update(unsafe.Pointer(&progID), unsafe.Pointer(&fd),
		cebpf.UpdateAny); err != nil {
		// Every eBPF program that is loaded within loadUnwinders can be the
		// destination of a tail call of another eBPF program. If we can not update
		// the eBPF map that manages these destinations our unwinding will fail.
		return fmt.Errorf("failed to update tailcall map: %v", err)
	}
	return nil
}

// insertKernelFrames fetches the kernel stack frames for a particular kstackID and populates
// the trace with these kernel frames. It also allocates the memory for the frames of the trace.
// It returns the number of kernel frames for kstackID or an error.
func (t *Tracer) insertKernelFrames(trace *host.Trace, ustackLen uint32,
	kstackID int32) (uint32, error) {
	cKstackID := kstackID
	kstackVal := make([]uint64, support.PerfMaxStackDepth)

	if err := t.ebpfMaps["kernel_stackmap"].Lookup(unsafe.Pointer(&cKstackID),
		unsafe.Pointer(&kstackVal[0])); err != nil {
		return 0, fmt.Errorf("failed to lookup kernel frames for stackID %d: %v", kstackID, err)
	}

	// The kernel returns absolute addresses in kernel address
	// space format. Here just the stack length is needed.
	// But also debug print the symbolization based on kallsyms.
	var kstackLen uint32
	for kstackLen < support.PerfMaxStackDepth && kstackVal[kstackLen] != 0 {
		kstackLen++
	}

	trace.Frames = make([]host.Frame, kstackLen+ustackLen)

	var kernelSymbolCacheHit, kernelSymbolCacheMiss uint64

	for i := uint32(0); i < kstackLen; i++ {
		fileID := libpf.UnknownKernelFileID
		address := libpf.Address(kstackVal[i])
		kmod, err := t.kernelSymbolizer.GetModuleByAddress(address)
		if err == nil {
			fileID = kmod.FileID()
			address -= kmod.Start()
		}

		hostFileID := host.FileIDFromLibpf(fileID)
		t.processManager.FileIDMapper.Set(hostFileID, fileID)

		trace.Frames[i] = host.Frame{
			File:   hostFileID,
			Lineno: libpf.AddressOrLineno(address),
			Type:   libpf.KernelFrame,

			// For all kernel frames, the kernel unwinder will always produce a
			// frame in which the RIP is after a call instruction (it hides the
			// top frames that leads to the unwinder itself).
			ReturnAddress: true,
		}

		// Kernel frame PCs need to be adjusted by -1. This duplicates logic done in the trace
		// converter. This should be fixed with PF-1042.
		frameID := libpf.NewFrameID(fileID, trace.Frames[i].Lineno-1)
		if t.reporter.FrameKnown(frameID) {
			kernelSymbolCacheHit++
			continue
		}
		kernelSymbolCacheMiss++

		if kmod == nil {
			continue
		}
		if funcName, _, err := kmod.LookupSymbolByAddress(libpf.Address(kstackVal[i])); err == nil {
			t.reporter.FrameMetadata(&reporter.FrameMetadataArgs{
				FrameID:      frameID,
				FunctionName: libpf.Intern(funcName),
			})
			t.reporter.ExecutableMetadata(&reporter.ExecutableMetadataArgs{
				FileID:     kmod.FileID(),
				FileName:   kmod.Name(),
				GnuBuildID: kmod.BuildID(),
				Interp:     libpf.Kernel,
			})
		}
	}

	t.fallbackSymbolMiss.Add(kernelSymbolCacheMiss)
	t.fallbackSymbolHit.Add(kernelSymbolCacheHit)

	return kstackLen, nil
}

// enableEvent removes the entry of given eventType from the inhibitEvents map
// so that the eBPF code will send the event again.
func (t *Tracer) enableEvent(eventType int) {
	inhibitEventsMap := t.ebpfMaps["inhibit_events"]

	// The map entry might not exist, so just ignore the potential error.
	et := uint32(eventType)
	_ = inhibitEventsMap.Delete(unsafe.Pointer(&et))
}

// monitorPIDEventsMap periodically iterates over the eBPF map pid_events,
// collects PIDs and writes them to the keys slice.
func (t *Tracer) monitorPIDEventsMap(keys *[]libpf.PIDTID) {
	eventsMap := t.ebpfMaps["pid_events"]
	var key, nextKey uint64
	var value bool
	keyFound := true
	deleteBatch := make(libpf.Set[uint64])

	// Key 0 retrieves the very first element in the hash map as
	// it is guaranteed not to exist in pid_events.
	key = 0
	if err := eventsMap.NextKey(unsafe.Pointer(&key), unsafe.Pointer(&nextKey)); err != nil {
		if errors.Is(err, cebpf.ErrKeyNotExist) {
			log.Debugf("Empty pid_events map")
			return
		}
		log.Fatalf("Failed to read from pid_events map: %v", err)
	}

	for keyFound {
		key = nextKey

		if err := eventsMap.Lookup(unsafe.Pointer(&key), unsafe.Pointer(&value)); err != nil {
			log.Fatalf("Failed to lookup '%v' in pid_events: %v", key, err)
		}

		// Lookup the next map entry before deleting the current one.
		if err := eventsMap.NextKey(unsafe.Pointer(&key), unsafe.Pointer(&nextKey)); err != nil {
			if !errors.Is(err, cebpf.ErrKeyNotExist) {
				log.Fatalf("Failed to read from pid_events map: %v", err)
			}
			keyFound = false
		}

		if !t.hasBatchOperations {
			// Now that we have the next key, we can delete the current one.
			if err := eventsMap.Delete(unsafe.Pointer(&key)); err != nil {
				log.Fatalf("Failed to delete '%v' from pid_events: %v", key, err)
			}
		} else {
			// Store to-be-deleted keys in a map so we can delete them all with a single
			// bpf syscall.
			deleteBatch[key] = libpf.Void{}
		}

		// If we process keys inline with iteration (e.g. by sending them to t.pidEvents at this
		// exact point), we may block sending to the channel, delay the iteration and may introduce
		// race conditions (related to deletion). For that reason, keys are first collected and,
		// after the iteration has finished, sent to the channel.
		*keys = append(*keys, libpf.PIDTID(key))
	}

	keysToDelete := len(deleteBatch)
	if keysToDelete != 0 {
		keys := libpf.MapKeysToSlice(deleteBatch)
		if _, err := eventsMap.BatchDelete(keys, nil); err != nil {
			log.Fatalf("Failed to batch delete %d entries from pid_events map: %v",
				keysToDelete, err)
		}
	}
}

// eBPFMetricsCollector retrieves the eBPF metrics, calculates their delta values,
// and translates eBPF IDs into Metric ID.
// Returns a slice of Metric ID/Value pairs.
func (t *Tracer) eBPFMetricsCollector(
	translateIDs []metrics.MetricID,
	previousMetricValue []metrics.MetricValue) []metrics.Metric {
	metricsMap := t.ebpfMaps["metrics"]
	metricsUpdates := make([]metrics.Metric, 0, len(translateIDs))

	// Iterate over all known metric IDs
	for ebpfID, metricID := range translateIDs {
		var perCPUValues []uint64

		// Checking for 'gaps' in the translation table.
		// That allows non-contiguous metric IDs, e.g. after removal/deprecation of a metric ID.
		if metricID == metrics.IDInvalid {
			continue
		}

		eID := uint32(ebpfID)
		if err := metricsMap.Lookup(unsafe.Pointer(&eID), &perCPUValues); err != nil {
			log.Errorf("Failed trying to lookup per CPU element: %v", err)
			continue
		}
		value := metrics.MetricValue(0)
		for _, val := range perCPUValues {
			value += metrics.MetricValue(val)
		}

		// The monitoring infrastructure expects instantaneous values (gauges).
		// => for cumulative metrics (counters), send deltas of the observed values, so they
		// can be interpreted as gauges.
		if ebpfID < support.MetricIDBeginCumulative {
			// We don't assume 64bit counters to overflow
			deltaValue := value - previousMetricValue[ebpfID]

			// 0 deltas add no value when summed up for display purposes in the UI
			if deltaValue == 0 {
				continue
			}

			previousMetricValue[ebpfID] = value
			value = deltaValue
		}

		// Collect the metrics for reporting
		metricsUpdates = append(metricsUpdates, metrics.Metric{
			ID:    metricID,
			Value: value,
		})
	}

	return metricsUpdates
}

// loadBpfTrace parses a raw BPF trace into a `host.Trace` instance.
//
// If the raw trace contains a kernel stack ID, the kernel stack is also
// retrieved and inserted at the appropriate position.
func (t *Tracer) loadBpfTrace(raw []byte, cpu int) *host.Trace {
	frameListOffs := int(unsafe.Offsetof(support.Trace{}.Frames))

	if len(raw) < frameListOffs {
		panic("trace record too small")
	}

	frameSize := support.Sizeof_Frame
	ptr := (*support.Trace)(unsafe.Pointer(unsafe.SliceData(raw)))

	// NOTE: can't do exact check here: kernel adds a few padding bytes to messages.
	if len(raw) < frameListOffs+int(ptr.Stack_len)*frameSize {
		panic("unexpected record size")
	}

	pid := libpf.PID(ptr.Pid)
	procMeta := t.processManager.MetaForPID(pid)
	trace := &host.Trace{
		Comm:             goString(ptr.Comm[:]),
		ExecutablePath:   procMeta.Executable,
		ContainerID:      procMeta.ContainerID,
		ProcessName:      procMeta.Name,
		APMTraceID:       *(*libpf.APMTraceID)(unsafe.Pointer(&ptr.Apm_trace_id)),
		APMTransactionID: *(*libpf.APMTransactionID)(unsafe.Pointer(&ptr.Apm_transaction_id)),
		PID:              pid,
		TID:              libpf.PID(ptr.Tid),
		Origin:           libpf.Origin(ptr.Origin),
		OffTime:          int64(ptr.Offtime),
		KTime:            times.KTime(ptr.Ktime),
		CPU:              cpu,
		EnvVars:          procMeta.EnvVariables,
	}

	if trace.Origin != support.TraceOriginSampling && trace.Origin != support.TraceOriginOffCPU {
		log.Warnf("Skip handling trace from unexpected %d origin", trace.Origin)
		return nil
	}

	// Trace fields included in the hash:
	//  - PID, kernel stack ID, length & frame array
	// Intentionally excluded:
	//  - ktime, COMM, APM trace, APM transaction ID, Origin and Off Time
	ptr.Comm = [16]byte{}
	ptr.Apm_trace_id = support.ApmTraceID{}
	ptr.Apm_transaction_id = support.ApmSpanID{}
	ptr.Ktime = 0
	ptr.Origin = 0
	ptr.Offtime = 0
	trace.Hash = host.TraceHash(xxh3.Hash128(raw).Lo)

	userFrameOffs := 0
	if ptr.Kernel_stack_id >= 0 {
		kstackLen, err := t.insertKernelFrames(
			trace, ptr.Stack_len, ptr.Kernel_stack_id)

		if err != nil {
			log.Errorf("Failed to get kernel stack frames for 0x%x: %v", trace.Hash, err)
		} else {
			userFrameOffs = int(kstackLen)
		}
	}

	if ptr.Custom_labels.Len > 0 {
		trace.CustomLabels = make(map[string]string, int(ptr.Custom_labels.Len))
		for i := 0; i < int(ptr.Custom_labels.Len); i++ {
			lbl := ptr.Custom_labels.Labels[i]
			key := goString(lbl.Key[:])
			val := goString(lbl.Val[:])
			trace.CustomLabels[key] = val
		}
	}

	// If there are no kernel frames, or reading them failed, we are responsible
	// for allocating the columnar frame array.
	if len(trace.Frames) == 0 {
		trace.Frames = make([]host.Frame, ptr.Stack_len)
	}

	for i := 0; i < int(ptr.Stack_len); i++ {
		rawFrame := &ptr.Frames[i]
		trace.Frames[userFrameOffs+i] = host.Frame{
			File:          host.FileID(rawFrame.File_id),
			Lineno:        libpf.AddressOrLineno(rawFrame.Addr_or_line),
			Type:          libpf.FrameType(rawFrame.Kind),
			ReturnAddress: rawFrame.Return_address != 0,
		}
	}
	return trace
}

// StartMapMonitors starts goroutines for collecting metrics and monitoring eBPF
// maps for tracepoints, new traces, trace count updates and unknown PCs.
func (t *Tracer) StartMapMonitors(ctx context.Context, traceOutChan chan<- *host.Trace) error {
	if err := t.kernelSymbolizer.StartMonitor(ctx); err != nil {
		log.Warnf("Failed to start kallsyms monitor: %v", err)
	}
	eventMetricCollector := t.startEventMonitor(ctx)
	traceEventMetricCollector := t.startTraceEventMonitor(ctx, traceOutChan)

	pidEvents := make([]libpf.PIDTID, 0)
	periodiccaller.StartWithManualTrigger(ctx, t.intervals.MonitorInterval(),
		t.triggerPIDProcessing, func(_ bool) {
			t.enableEvent(support.EventTypeGenericPID)
			t.monitorPIDEventsMap(&pidEvents)

			for _, pidTid := range pidEvents {
				log.Debugf("=> %v", pidTid)
				t.pidEvents <- pidTid
			}

			// Keep the underlying array alive to avoid GC pressure
			pidEvents = pidEvents[:0]
		})

	// translateIDs is a translation table for eBPF IDs into Metric IDs.
	// Index is the ebpfID, value is the corresponding metricID.
	translateIDs := support.MetricsTranslation

	// previousMetricValue stores the previously retrieved metric values to
	// calculate and store delta values.
	previousMetricValue := make([]metrics.MetricValue, len(translateIDs))

	periodiccaller.Start(ctx, t.intervals.MonitorInterval(), func() {
		metrics.AddSlice(eventMetricCollector())
		metrics.AddSlice(traceEventMetricCollector())
		metrics.AddSlice(t.eBPFMetricsCollector(translateIDs, previousMetricValue))

		metrics.AddSlice([]metrics.Metric{
			{
				ID:    metrics.IDKernelFallbackSymbolLRUHit,
				Value: metrics.MetricValue(t.fallbackSymbolHit.Swap(0)),
			},
			{
				ID:    metrics.IDKernelFallbackSymbolLRUMiss,
				Value: metrics.MetricValue(t.fallbackSymbolMiss.Swap(0)),
			},
		})
	})

	return nil
}

// AttachTracer attaches the main tracer entry point to the perf interrupt events. The tracer
// entry point is always the native tracer. The native tracer will determine when to invoke the
// interpreter tracers based on address range information.
func (t *Tracer) AttachTracer() error {
	tracerProg, ok := t.ebpfProgs["native_tracer_entry"]
	if !ok {
		return errors.New("entry program is not available")
	}

	perfAttribute := new(perf.Attr)
	perfAttribute.SetSampleFreq(uint64(t.samplesPerSecond))
	if err := perf.CPUClock.Configure(perfAttribute); err != nil {
		return fmt.Errorf("failed to configure software perf event: %v", err)
	}

	onlineCPUIDs, err := getOnlineCPUIDs()
	if err != nil {
		return fmt.Errorf("failed to get online CPUs: %v", err)
	}

	events := t.perfEntrypoints.WLock()
	defer t.perfEntrypoints.WUnlock(&events)
	for _, id := range onlineCPUIDs {
		perfEvent, err := perf.Open(perfAttribute, perf.AllThreads, id, nil)
		if err != nil {
			return fmt.Errorf("failed to attach to perf event on CPU %d: %v", id, err)
		}
		if err := perfEvent.SetBPF(uint32(tracerProg.FD())); err != nil {
			return fmt.Errorf("failed to attach eBPF program to perf event: %v", err)
		}
		*events = append(*events, perfEvent)
	}
	return nil
}

// EnableProfiling enables the perf interrupt events with the attached eBPF programs.
func (t *Tracer) EnableProfiling() error {
	events := t.perfEntrypoints.WLock()
	defer t.perfEntrypoints.WUnlock(&events)
	if len(*events) == 0 {
		return errors.New("no perf events available to enable for profiling")
	}
	for id, event := range *events {
		if err := event.Enable(); err != nil {
			return fmt.Errorf("failed to enable perf event on CPU %d: %v", id, err)
		}
	}
	return nil
}

// probabilisticProfile performs a single iteration of probabilistic profiling. It will generate
// a random number between 0 and ProbabilisticThresholdMax-1 every interval. If the random
// number is smaller than threshold it will enable the frequency based sampling for this
// time interval. Otherwise the frequency based sampling events are disabled.
func (t *Tracer) probabilisticProfile(interval time.Duration, threshold uint) {
	enableSampling := false
	var probProfilingStatus = probProfilingDisable

	//nolint:gosec
	if rand.UintN(ProbabilisticThresholdMax) < threshold {
		enableSampling = true
		probProfilingStatus = probProfilingEnable
		log.Debugf("Start sampling for next interval (%v)", interval)
	} else {
		log.Debugf("Stop sampling for next interval (%v)", interval)
	}

	events := t.perfEntrypoints.WLock()
	defer t.perfEntrypoints.WUnlock(&events)
	var enableErr, disableErr metrics.MetricValue
	for _, event := range *events {
		if enableSampling {
			if err := event.Enable(); err != nil {
				enableErr++
				log.Errorf("Failed to enable frequency based sampling: %v",
					err)
			}
			continue
		}
		if err := event.Disable(); err != nil {
			disableErr++
			log.Errorf("Failed to disable frequency based sampling: %v", err)
		}
	}
	if enableErr != 0 {
		metrics.Add(metrics.IDPerfEventEnableErr, enableErr)
	}
	if disableErr != 0 {
		metrics.Add(metrics.IDPerfEventDisableErr, disableErr)
	}
	metrics.Add(metrics.IDProbProfilingStatus,
		metrics.MetricValue(probProfilingStatus))
}

// StartProbabilisticProfiling periodically runs probabilistic profiling.
func (t *Tracer) StartProbabilisticProfiling(ctx context.Context) {
	metrics.Add(metrics.IDProbProfilingInterval,
		metrics.MetricValue(t.probabilisticInterval.Seconds()))

	// Run a single iteration of probabilistic profiling to avoid needing
	// to wait for the first interval to pass with periodiccaller.Start()
	// before getting called.
	t.probabilisticProfile(t.probabilisticInterval, t.probabilisticThreshold)

	periodiccaller.Start(ctx, t.probabilisticInterval, func() {
		t.probabilisticProfile(t.probabilisticInterval, t.probabilisticThreshold)
	})
}

// StartOffCPUProfiling starts off-cpu profiling by attaching the programs to the hooks.
func (t *Tracer) StartOffCPUProfiling() error {
	// Attach the second hook for off-cpu profiling first.
	kprobeProg, ok := t.ebpfProgs["finish_task_switch"]
	if !ok {
		return errors.New("off-cpu program finish_task_switch is not available")
	}

	kmod, err := t.kernelSymbolizer.GetModuleByName(kallsyms.Kernel)
	if err != nil {
		return err
	}

	hookSymbolPrefix := "finish_task_switch"
	kprobeSymbs := kmod.LookupSymbolsByPrefix(hookSymbolPrefix)
	if len(kprobeSymbs) == 0 {
		return errors.New("no finish_task_switch symbols found")
	}

	attached := false
	// Attach to all symbols with the prefix finish_task_switch.
	for _, symb := range kprobeSymbs {
		kprobeLink, linkErr := link.Kprobe(string(symb.Name), kprobeProg, nil)
		if linkErr != nil {
			log.Warnf("Failed to attach to %s: %v", symb.Name, linkErr)
			continue
		}
		attached = true
		t.hooks[hookPoint{group: "kprobe", name: string(symb.Name)}] = kprobeLink
	}
	if !attached {
		return fmt.Errorf("failed to attach to one of %d symbols with prefix '%s'",
			len(kprobeSymbs), hookSymbolPrefix)
	}

	// Attach the first hook that enables off-cpu profiling.
	tpProg, ok := t.ebpfProgs["tracepoint__sched_switch"]
	if !ok {
		return errors.New("tracepoint__sched_switch is not available")
	}
	tpLink, err := link.Tracepoint("sched", "sched_switch", tpProg, nil)
	if err != nil {
		return nil
	}
	t.hooks[hookPoint{group: "sched", name: "sched_switch"}] = tpLink

	return nil
}

// TraceProcessor gets the trace processor.
func (t *Tracer) TraceProcessor() tracehandler.TraceProcessor {
	return t.processManager
}
