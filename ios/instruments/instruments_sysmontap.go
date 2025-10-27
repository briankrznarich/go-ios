package instruments

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	log "github.com/sirupsen/logrus"
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
	// Close() called, p.messages no longer being processed, discard subsequent messages
	case <-p.ctx.Done():
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
		return nil, err
	}

	processControlChannel := dtxConn.RequestChannelIdentifier(sysmontapName, loggingDispatcher{dtxConn})

	sysAttrs, err := deviceInfoService.systemAttributes()
	if err != nil {
		return nil, err
	}

	procAttrs, err := deviceInfoService.processAttributes()
	if err != nil {
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
		return nil, err
	}

	err = processControlChannel.MethodCallAsync("start")
	if err != nil {
		return nil, err
	}

	return &sysmontapService{
		processControlChannel, dtxConn, deviceInfoService, msgDispatcher, ctx, cancel,
	}, nil
}

// Close closes up the DTX connection and message dispatcher
func (s *sysmontapService) Close() error {
	s.cancel()
	s.deviceInfoService.Close()
	return s.conn.Close()
}

// ReceiveCPUUsage returns a chan of SysmontapMessage with CPU Usage info
// The method will close the result channel automatically as soon as sysmontapMsgDispatcher's
// dtx.Message channel is closed.
func (s *sysmontapService) ReceiveCPUUsage() chan SysmontapMessage {
	messages := make(chan SysmontapMessage)
	go func() {
		defer close(messages)

		for msg := range s.msgDispatcher.messages {
			sysmontapMessage, err := mapToCPUUsage(msg)
			if err != nil {
				log.Debugf("expected `sysmontapMessage` from global channel, but received %v", msg)
				continue
			}

			messages <- sysmontapMessage
		}

		log.Infof("sysmontap message dispatcher channel closed")
	}()

	return messages
}

// GetSystemMetrics returns a single SysmontapMessage with CPU and Memory info. Currently
// only the first message in s.msgDispatcher.messages contains memory information, so you can't
// meaningfully use a continuous tap if you want memory data.
func GetSystemMetrics(device ios.DeviceEntry, cpuSampleDelay int) (SysmontapMessage, error) {
	sysmon, err := NewSysmontapService(device, 10)
	if err != nil {
		return SysmontapMessage{}, err
	}
	defer sysmon.Close()

	firstMessage := SysmontapMessage{}
	messagesReceived := 0

	for msg := range sysmon.msgDispatcher.messages {
		cpuDataOnly := messagesReceived > 0
		sysmontapMessage, err := mapToCPUAndSystemMetrics(msg, cpuDataOnly)
		if err != nil {
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
		return firstMessage, nil
	}
	return SysmontapMessage{}, errors.New("sysmontapMessage channel closed with no messages received")
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

func mapToCPUUsage(msg dtx.Message) (SysmontapMessage, error) {
	return mapToCPUAndSystemMetrics(msg, true)
}

func mapToCPUAndSystemMetrics(msg dtx.Message, cpuDataOnly bool) (SysmontapMessage, error) {
	payload := msg.Payload
	if len(payload) != 1 {
		return SysmontapMessage{}, fmt.Errorf("payload of message should have only one element: %+v", msg)
	}

	resultArray, ok := payload[0].([]interface{})
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected resultArray of type []interface{}: %+v", payload[0])
	}
	resultMap, ok := resultArray[0].(map[string]interface{})
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected resultMap of type map[string]interface{} as a single element of resultArray: %+v", resultArray[0])
	}
	cpuCount, ok := resultMap["CPUCount"].(uint64)
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected CPUCount of type uint64 of resultMap: %+v", resultMap)
	}
	enabledCPUs, ok := resultMap["EnabledCPUs"].(uint64)
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected EnabledCPUs of type uint64 of resultMap: %+v", resultMap)
	}
	endMachAbsTime, ok := resultMap["EndMachAbsTime"].(uint64)
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected EndMachAbsTime of type uint64 of resultMap: %+v", resultMap)
	}
	typ, ok := resultMap["Type"].(uint64)
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected Type of type uint64 of resultMap: %+v", resultMap)
	}
	sysmontapMessageMap, ok := resultMap["SystemCPUUsage"].(map[string]interface{})
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected SystemCPUUsage of type map[string]interface{} of resultMap: %+v", resultMap)
	}
	cpuTotalLoad, ok := sysmontapMessageMap["CPU_TotalLoad"].(float64)
	if !ok {
		return SysmontapMessage{}, fmt.Errorf("expected CPU_TotalLoad of type uint64 of sysmontapMessageMap: %+v", sysmontapMessageMap)
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

	if !okA || !okS || len(systemAttributesRaw) != len(systemValuesRaw) {
		return sysmontapMessage, nil
	}

	rawMetrics := make(map[string]uint64)
	for i, keyRaw := range systemAttributesRaw {
		key, ok := keyRaw.(string)
		if !ok {
			// If the array exists, it should be well-formed.
			return SysmontapMessage{}, fmt.Errorf("SystemAttributes key at index %d is not a string: %v", i, keyRaw)
		}

		// Values can come in as various number types (int, int64, float64, etc.)
		// We'll normalize them to int64.
		var uintVal uint64
		valRaw := systemValuesRaw[i]
		switch v := valRaw.(type) {
		case int64:
			uintVal = uint64(v)
		case uint64:
			uintVal = uint64(v)
		default:
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
		"__vmSwapUsage",
	}

	// Check that all keys exist
	for _, key := range requiredKeys {
		if _, ok := rawMetrics[key]; !ok {
			log.Errorf("SystemAttributes missing required key '%s' ", key)
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

	// For details, look up "struct vm_statistics" and/or vm_statistics64_data_t
	appMemPages := rawMetrics["vmIntPageCount"] // - rawMetrics["vmPurgeableCount"]

	// With this calculation, free + cached + user = Total.  May not match iOS task manager exactly
	freePages := rawMetrics["vmFreeCount"]
	cachedPages := rawMetrics["vmExtPageCount"] + rawMetrics["vmPurgeableCount"]
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
