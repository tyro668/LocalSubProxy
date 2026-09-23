package connector

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

var deviceNameCache struct {
	sync.Once
	name string
}

// localDeviceName is a display label. The remote service device ID remains an opaque
// protocol identifier and must not change when the computer is renamed.
func localDeviceName() string {
	deviceNameCache.Do(func() {
		switch runtime.GOOS {
		case "darwin":
			if value, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
				deviceNameCache.name = strings.TrimSpace(string(value))
			}
		case "windows":
			deviceNameCache.name = strings.TrimSpace(os.Getenv("COMPUTERNAME"))
		}
		if deviceNameCache.name == "" {
			deviceNameCache.name, _ = os.Hostname()
		}
	})
	return deviceNameCache.name
}
