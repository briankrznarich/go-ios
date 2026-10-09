package instruments

import (
	"context"
	"fmt"
	"math"
	"reflect"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/golog"
)

type sysmontapMsgDispatcher struct {
	messages chan dtx.Message
	ctx      context.Context
}

func newSysmontapMsgDispatcher(ctx context.Context) *sysmontapMsgDispatcher {
	return &sysmontapMsgDispatcher{
		messages: make(chan dtx.Message),
		ctx:      ctx,
	}
}

func (p *sysmontapMsgDispatcher) Dispatch(m dtx.Message) {
	select {
	case p.messages <- m:
	case <-p.ctx.Done():
		// Close must release pending dispatches even if nobody is receiving.
	}
}

const sysmontapName = "com.apple.instruments.server.services.sysmontap"

type sysmontapService struct {
	channel *dtx.Channel
	conn    *dtx.Connection

	deviceInfoService *DeviceInfoService
	msgDispatcher     *sysmontapMsgDispatcher

	ctx    context.Context
	cancel context.CancelFunc
}

// NewSysmontapService creates a new sysmontapService
// - samplingInterval is the rate how often to get samples, i.e Xcode's default is 10, which results in sampling output
// each 1 second, with 500 the samples are retrieved every 15 seconds. It doesn't make any correlation between
// the expected rate and the actual rate of samples delivery. We can only conclude, that the lower the rate in digits,
// the faster the samples are delivered
func NewSysmontapService(device ios.DeviceEntry, samplingInterval int) (*sysmontapService, error) {
	deviceInfoService, err := NewDeviceInfoService(device)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	msgDispatcher := newSysmontapMsgDispatcher(ctx)
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, msgDispatcher)
	if err != nil {
		cancel()
		deviceInfoService.Close()
		return nil, err
	}

	processControlChannel := dtxConn.RequestChannelIdentifier(sysmontapName, loggingDispatcher{dtxConn})

	sysAttrs, err := deviceInfoService.systemAttributes()
	if err != nil {
		cancel()
		deviceInfoService.Close()
		dtxConn.Close()
		return nil, err
	}

	procAttrs, err := deviceInfoService.processAttributes()
	if err != nil {
		cancel()
		deviceInfoService.Close()
		dtxConn.Close()
		return nil, err
	}

	config := map[string]interface{}{
		"ur":             samplingInterval,
		"bm":             0,
		"procAttrs":      procAttrs,
		"sysAttrs":       sysAttrs,
		"cpuUsage":       true,
		"physFootprint":  true,
		"sampleInterval": 500000000,
	}
	_, err = processControlChannel.MethodCall("setConfig:", config)
	if err != nil {
		cancel()
		deviceInfoService.Close()
		dtxConn.Close()
		return nil, err
	}

	err = processControlChannel.MethodCallAsync("start")
	if err != nil {
		cancel()
		deviceInfoService.Close()
		dtxConn.Close()
		return nil, err
	}

	return &sysmontapService{processControlChannel, dtxConn, deviceInfoService, msgDispatcher, ctx, cancel}, nil
}

// Close cancels dispatch and receive operations before closing the DTX connections.
func (s *sysmontapService) Close() error {
	s.cancel()

	s.deviceInfoService.Close()
	return s.conn.Close()
}

// ReceiveCPUUsage returns a chan of SysmontapMessage with CPU Usage info
// The result channel is closed when the service is closed.
func (s *sysmontapService) ReceiveCPUUsage() chan SysmontapMessage {
	messages := make(chan SysmontapMessage)
	go func() {
		defer close(messages)
		defer golog.Info("sysmontap receiver stopped", "module", logModule)

		for {
			var msg dtx.Message
			select {
			case msg = <-s.msgDispatcher.messages:
			case <-s.ctx.Done():
				return
			}
			sysmontapMessage, err := mapToCPUUsage(msg)
			if err != nil {
				golog.Debug("expected `sysmontapMessage` from global channel, but received different message", "module", logModule, "message", msg)
				continue
			}

			select {
			case messages <- sysmontapMessage:
			case <-s.ctx.Done():
				return
			}
		}
	}()

	return messages
}

// GetSystemMetrics returns a single SysmontapMessage with CPU and Memory info. Currently
// only the first message in s.msgDispatcher.messages contains memory information, so you can't
// meaningfully use a continuous tap if you want memory data.
// cpuSampleDelay is the number of CPU samples to skip, not a duration.
func GetSystemMetrics(device ios.DeviceEntry, cpuSampleDelay int) (SysmontapMessage, error) {
	if cpuSampleDelay < 0 {
		return SysmontapMessage{}, fmt.Errorf("cpuSampleDelay must not be negative")
	}
	sysmon, err := NewSysmontapService(device, 10)
	if err != nil {
		return SysmontapMessage{}, err
	}
	defer sysmon.Close()

	firstMessage := SysmontapMessage{}
	messagesReceived := 0

	for {
		var msg dtx.Message
		select {
		case msg = <-sysmon.msgDispatcher.messages:
		case <-sysmon.conn.Closed():
			return SysmontapMessage{}, fmt.Errorf("sysmontap connection closed before collecting system metrics: %w", sysmon.conn.Err())
		}
		cpuDataOnly := messagesReceived > 0
		sysmontapMessage, err := mapToCPUAndSystemMetrics(msg, cpuDataOnly)
		if err != nil {
			// Ignore unrelated messages, but report malformed system data in a CPU sample.
			if _, cpuErr := mapToCPUUsage(msg); cpuErr == nil {
				return SysmontapMessage{}, err
			}
			continue
		}
		// As a rule, first CPU_TotalLoad message is garbage data, often 0. Second message often inflated by
		// our efforts setting up the query.
		if messagesReceived == 0 {
			firstMessage = sysmontapMessage
		}
		if messagesReceived < cpuSampleDelay {
			messagesReceived += 1
			continue
		}
		firstMessage.SystemCPUUsage.CPU_TotalLoad = sysmontapMessage.SystemCPUUsage.CPU_TotalLoad
		// Keep the timestamp and CPU metadata with the CPU reading they describe.
		firstMessage.CPUCount = sysmontapMessage.CPUCount
		firstMessage.EnabledCPUs = sysmontapMessage.EnabledCPUs
		firstMessage.EndMachAbsTime = sysmontapMessage.EndMachAbsTime
		firstMessage.Type = sysmontapMessage.Type
		return firstMessage, nil
	}
}

// MemoryUsage holds computed memory metrics in bytes.
// A value of 0 can indicate "unavailable" or "zero".
type MemoryUsage struct {
	Total       uint64 // Installed memory
	Free        uint64 // Unallocated
	Cache       uint64 // Cached Files + Discardable
	Available   uint64 // Free + Cache
	Compressed  uint64
	Used        uint64 // Total - Free - Cached
	Wired       uint64
	Swap        uint64
	Application uint64
}

// SysmontapMessage is a wrapper struct for incoming CPU samples
type SysmontapMessage struct {
	CPUCount         uint64
	EnabledCPUs      uint64
	EndMachAbsTime   uint64
	Type             uint64
	SystemCPUUsage   CPUUsage
	RawSystemMetrics map[string]uint64 `json:",omitempty"`
	MemoryUsage      MemoryUsage
}

type CPUUsage struct {
	CPU_TotalLoad float64
}

// toUint64 converts any numeric type to uint64. Returns false for
// non-numeric types or negative values.
func toUint64(v interface{}) (uint64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := rv.Int()
		if i < 0 {
			return 0, false
		}
		return uint64(i), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if f < 0 {
			return 0, false
		}
		return uint64(f), true
	default:
		return 0, false
	}
}

// toFloat64 converts any numeric type to float64. Returns false for
// non-numeric types.
func toFloat64(v interface{}) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	default:
		return 0, false
	}
}

// requireUint64 extracts a uint64 from a map field, accepting any numeric type.
func requireUint64(m map[string]interface{}, key string) (uint64, error) {
	v, ok := toUint64(m[key])
	if !ok {
		return 0, fmt.Errorf("expected numeric %s, got %T: %+v", key, m[key], m[key])
	}
	return v, nil
}

// requireFloat64 extracts a float64 from a map field, accepting any numeric type.
func requireFloat64(m map[string]interface{}, key string) (float64, error) {
	v, ok := toFloat64(m[key])
	if !ok {
		return 0, fmt.Errorf("expected numeric %s, got %T: %+v", key, m[key], m[key])
	}
	return v, nil
}

// requireMap extracts a map[string]interface{} from a map field.
func requireMap(m map[string]interface{}, key string) (map[string]interface{}, error) {
	raw, exists := m[key]
	if !exists {
		return nil, fmt.Errorf("%s missing in result map", key)
	}
	sub, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected map[string]interface{} for %s, got %T", key, raw)
	}
	return sub, nil
}

// extractResultMap unwraps the DTX payload into the first result map.
// Payload structure: []interface{ []interface{ map[string]interface{}, ... }, ... }
func extractResultMap(payload []interface{}) (map[string]interface{}, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload in sysmontap message")
	}

	resultArray, ok := payload[0].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{} as payload[0], got %T: %+v", payload[0], payload[0])
	}
	if len(resultArray) == 0 {
		return nil, fmt.Errorf("result array is empty in sysmontap payload: %+v", payload)
	}

	resultMap, ok := resultArray[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected map[string]interface{} for result, got %T: %+v", resultArray[0], resultArray[0])
	}
	return resultMap, nil
}

// mapToCPUUsage parses a DTX sysmontap message into a SysmontapMessage.
// It tolerates numeric type variations (int, int64, uint32, float64, etc.)
// across different iOS versions and device types.
func mapToCPUUsage(msg dtx.Message) (SysmontapMessage, error) {
	return mapToCPUAndSystemMetrics(msg, true)
}

func mapToCPUAndSystemMetrics(msg dtx.Message, cpuDataOnly bool) (SysmontapMessage, error) {
	resultMap, err := extractResultMap(msg.Payload)
	if err != nil {
		return SysmontapMessage{}, err
	}

	cpuCount, err := requireUint64(resultMap, "CPUCount")
	if err != nil {
		return SysmontapMessage{}, err
	}
	enabledCPUs, err := requireUint64(resultMap, "EnabledCPUs")
	if err != nil {
		return SysmontapMessage{}, err
	}
	endMachAbsTime, err := requireUint64(resultMap, "EndMachAbsTime")
	if err != nil {
		return SysmontapMessage{}, err
	}
	typ, err := requireUint64(resultMap, "Type")
	if err != nil {
		return SysmontapMessage{}, err
	}

	sysCPUMap, err := requireMap(resultMap, "SystemCPUUsage")
	if err != nil {
		return SysmontapMessage{}, err
	}
	cpuTotalLoad, err := requireFloat64(sysCPUMap, "CPU_TotalLoad")
	if err != nil {
		return SysmontapMessage{}, err
	}

	cpuUsage := CPUUsage{CPU_TotalLoad: cpuTotalLoad}

	sysmontapMessage := SysmontapMessage{
		CPUCount:       cpuCount,
		EnabledCPUs:    enabledCPUs,
		EndMachAbsTime: endMachAbsTime,
		Type:           typ,
		SystemCPUUsage: cpuUsage,
	}

	if cpuDataOnly {
		return sysmontapMessage, nil
	}

	systemAttributesRaw, okA := resultMap["SystemAttributes"].([]interface{})
	systemValuesRaw, okS := resultMap["System"].([]interface{})

	if !okA || !okS || len(systemAttributesRaw) == 0 || len(systemAttributesRaw) != len(systemValuesRaw) {
		return SysmontapMessage{}, fmt.Errorf("sysmontap sample has missing or mismatched SystemAttributes and System values")
	}

	rawMetrics := make(map[string]uint64)
	for i, keyRaw := range systemAttributesRaw {
		key, ok := keyRaw.(string)
		if !ok {
			// If the array exists, it should be well-formed.
			return SysmontapMessage{}, fmt.Errorf("SystemAttributes key at index %d is not a string: %v", i, keyRaw)
		}

		// Values can come in as various number types (int, int64, float64, etc.)
		// We'll normalize them to uint64.
		valRaw := systemValuesRaw[i]
		switch valRaw.(type) {
		case float32, float64:
			v, _ := toFloat64(valRaw)
			if math.IsNaN(v) || v < 0 || v >= 0x1p64 || math.Trunc(v) != v {
				continue
			}
		}
		uintVal, ok := toUint64(valRaw)
		if !ok {
			// Skip unlikely stats that aren't positive integers instead of crashing
			continue
		}
		rawMetrics[key] = uintVal
	}
	sysmontapMessage.RawSystemMetrics = rawMetrics

	// Compute MemoryUsage struct from RawSystemMetrics
	requiredKeys := []string{
		"vmIntPageCount",
		"vmPurgeableCount",
		"vmExtPageCount",
		"vmCompressorPageCount",
		"vmUsedCount",
		"vmWireCount",
		"vmFreeCount",
		"physMemSize",
	}

	// Check that all keys exist
	for _, key := range requiredKeys {
		if _, ok := rawMetrics[key]; !ok {
			golog.Warn("SystemAttributes missing required key", "module", logModule, "key", key)
			// Memory computations will be returned as zeros, but whatever raw data is available
			// and all CPU data will be returned while logging the error
			return sysmontapMessage, nil
		}
	}
	// True for 64bit arm, iPhone6+
	// This page size is needed to make any sense of the raw metrics, we may as well make it available.
	// If it ever changes, we will need to add code somehow to detect it, or the calculations will
	// all be incorrect.
	vmPageSize := uint64(16384)
	rawMetrics["__vmPageSize"] = vmPageSize

	// Check before converting page counts to bytes or subtracting cached pages.
	maxPages := uint64(math.MaxUint64) / vmPageSize
	for _, key := range requiredKeys {
		if rawMetrics[key] > maxPages {
			golog.Warn("SystemAttributes value overflows bytes", "module", logModule, "key", key)
			return sysmontapMessage, nil
		}
	}

	// For details, look up "struct vm_statistics" and/or vm_statistics64_data_t
	appMemPages := rawMetrics["vmIntPageCount"] // - rawMetrics["vmPurgeableCount"]

	// With this calculation, free + cached + used = Total
	// May not match iOS task manager exactly.
	freePages := rawMetrics["vmFreeCount"]
	cachedPages := rawMetrics["vmExtPageCount"] + rawMetrics["vmPurgeableCount"]
	if cachedPages > maxPages || freePages > maxPages-cachedPages {
		golog.Warn("cached or available memory overflows bytes", "module", logModule)
		return sysmontapMessage, nil
	}
	if cachedPages > rawMetrics["vmUsedCount"] {
		golog.Warn("cached memory exceeds vmUsedCount", "module", logModule)
		return sysmontapMessage, nil
	}
	memUsedPages := rawMetrics["vmUsedCount"] - rawMetrics["vmExtPageCount"] - rawMetrics["vmPurgeableCount"]

	compressedPages := rawMetrics["vmCompressorPageCount"]
	wiredPages := rawMetrics["vmWireCount"]
	totalMemPages := rawMetrics["physMemSize"]
	swapBytes := rawMetrics["__vmSwapUsage"] // This one is already in bytes

	memUsage := MemoryUsage{
		Total:       totalMemPages * vmPageSize,
		Free:        freePages * vmPageSize,
		Cache:       cachedPages * vmPageSize,
		Compressed:  compressedPages * vmPageSize,
		Used:        memUsedPages * vmPageSize,
		Wired:       wiredPages * vmPageSize,
		Application: appMemPages * vmPageSize,
		Swap:        swapBytes,
	}

	memUsage.Available = memUsage.Free + memUsage.Cache

	sysmontapMessage.MemoryUsage = memUsage

	return sysmontapMessage, nil
}
