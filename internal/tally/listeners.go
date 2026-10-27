package tally

import (
	"encoding/binary"
	"sort"
)

// tcpListener is one listening TCP socket and the process that owns it.
type tcpListener struct {
	Port int
	PID  uint32
}

// parseTCPOwnerPIDTable decodes a MIB_TCPTABLE_OWNER_PID buffer as Windows'
// GetExtendedTcpTable returns it: a DWORD row count, then rows of six DWORDs
// (state, local address, local port, remote address, remote port, owning
// PID), little-endian. The port sits in the low 16 bits in network byte
// order. Rows past the end of the buffer are ignored.
func parseTCPOwnerPIDTable(buf []byte) []tcpListener {
	const rowSize = 24
	if len(buf) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(buf[0:4]))
	out := make([]tcpListener, 0, n)
	for i := 0; i < n; i++ {
		off := 4 + i*rowSize
		if off+rowSize > len(buf) {
			break
		}
		raw := binary.LittleEndian.Uint32(buf[off+8 : off+12])
		port := int(uint16(raw&0xff)<<8 | uint16(raw>>8&0xff))
		pid := binary.LittleEndian.Uint32(buf[off+20 : off+24])
		out = append(out, tcpListener{Port: port, PID: pid})
	}
	return out
}

// tallyPorts keeps the ports whose owning process is tally.exe (not the
// licence gateway or scheduler, which also listen), sorted and unique.
func tallyPorts(listeners []tcpListener, imageName func(pid uint32) string) []int {
	names := map[uint32]string{}
	seen := map[int]bool{}
	var ports []int
	for _, l := range listeners {
		name, ok := names[l.PID]
		if !ok {
			name = imageName(l.PID)
			names[l.PID] = name
		}
		if !isTallyExe(name) || seen[l.Port] {
			continue
		}
		seen[l.Port] = true
		ports = append(ports, l.Port)
	}
	sort.Ints(ports)
	return ports
}

func isTallyExe(imageName string) bool {
	return len(imageName) == len("tally.exe") && equalFoldASCII(imageName, "tally.exe")
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
