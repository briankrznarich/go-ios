package ios

import log "github.com/sirupsen/logrus"
import "fmt"

const accessibilityDomain = "com.apple.Accessibility"
const assistiveTouchKey  = "AssistiveTouchEnabledByiTunes"

// EnableAssistiveTouch creates a new lockdown session for the device and enables or disables 
// AssistiveTouch (the on-screen software home button), by using the special key AssistiveTouchEnabledByiTunes.
// Setting to true will enable AssistiveTouch
// Setting to false will disable AssistiveTouch, regardless of whether it was previously-enabled through
// a non-iTunes-related method.
func SetAssistiveTouch(device DeviceEntry, enabled bool) error {
	lockDownConn, err := ConnectLockdownWithSession(device)
	if err != nil {
		return err
	}
	log.Debugf("Setting %s: %s", assistiveTouchKey, enabled)
	defer lockDownConn.Close()
	err = lockDownConn.SetValueForDomain(assistiveTouchKey, accessibilityDomain, enabled)
	return err
}
func GetAssistiveTouch(device DeviceEntry) (bool, error) {
	lockDownConn, err := ConnectLockdownWithSession(device)
	if err != nil {
		return false, err
	}
	defer lockDownConn.Close()
	enabledIntf, err := lockDownConn.GetValueForDomain(assistiveTouchKey, accessibilityDomain)
	if err != nil {
		return false, err
	}
	enabledUint64, ok := enabledIntf.(uint64)
	if !ok {
		return false, fmt.Errorf("Expected uint64 for key %s.%s, received %T: %+v instead!", accessibilityDomain, assistiveTouchKey, enabledIntf, enabledIntf)
	} else if enabledUint64 != 0 && enabledUint64 != 1{
		return false, fmt.Errorf("Expected a value of 0 or 1 for key %s.%s, received %d instead!", accessibilityDomain, assistiveTouchKey, enabledUint64)
	}
	return enabledUint64 == 1, nil
}
