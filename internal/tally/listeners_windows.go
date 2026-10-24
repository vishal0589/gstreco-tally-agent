//go:build windows

package tally

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	afINET                   = 2
	tcpTableOwnerPIDListener = 3 // TCP_TABLE_OWNER_PID_LISTENER
	errorInsufficientBuffer  = 122
)

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// TallyListeningPorts returns the TCP ports tally.exe processes on this
// machine are listening on: every Tally XML server, whatever port each
// install's tally.ini gives it. Reads the OS socket table directly (no
// netstat subprocess).
func TallyListeningPorts() ([]int, error) {
	var size uint32
	var buf []byte
	for attempt := 0; attempt < 4; attempt++ {
		var ptr uintptr
		if len(buf) > 0 {
			ptr = uintptr(unsafe.Pointer(&buf[0]))
		}
		r, _, _ := procGetExtendedTcpTable.Call(ptr, uintptr(unsafe.Pointer(&size)), 0, afINET, tcpTableOwnerPIDListener, 0)
		if r == 0 && len(buf) > 0 {
			// The row count leads the buffer, so trailing slack is ignored.
			return tallyPorts(parseTCPOwnerPIDTable(buf), processImageName), nil
		}
		if r != 0 && r != errorInsufficientBuffer {
			return nil, fmt.Errorf("GetExtendedTcpTable: error %d", r)
		}
		buf = make([]byte, size+1024) // the table can grow between calls
		size = uint32(len(buf))
	}
	return nil, fmt.Errorf("GetExtendedTcpTable: table kept growing")
}

func processImageName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}
