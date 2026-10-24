package tally

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func tcpRow(port int, pid uint32) []byte {
	row := make([]byte, 24)
	binary.LittleEndian.PutUint32(row[0:4], 2) // MIB_TCP_STATE_LISTEN
	// Network byte order in the low 16 bits of a little-endian DWORD.
	binary.LittleEndian.PutUint32(row[8:12], uint32(port>>8&0xff)|uint32(port&0xff)<<8)
	binary.LittleEndian.PutUint32(row[20:24], pid)
	return row
}

func TestParseTCPOwnerPIDTable(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, 3)
	buf = append(buf, tcpRow(9012, 1348)...)
	buf = append(buf, tcpRow(10011, 1348)...)
	buf = append(buf, tcpRow(9559, 35728)...)
	buf = append(buf, make([]byte, 40)...) // slack past the rows
	got := parseTCPOwnerPIDTable(buf)
	want := []tcpListener{{9012, 1348}, {10011, 1348}, {9559, 35728}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if parseTCPOwnerPIDTable(buf[:30]) == nil || len(parseTCPOwnerPIDTable(buf[:30])) != 1 {
		t.Error("a truncated buffer keeps only complete rows")
	}
}

func TestTallyPorts_KeepsOnlyTallyExe(t *testing.T) {
	names := map[uint32]string{1: "tally.exe", 2: "tallygatewayserver.exe", 3: "TALLY.EXE", 4: "tallyscheduler.exe"}
	got := tallyPorts([]tcpListener{{9559, 1}, {10010, 2}, {9012, 3}, {10093, 4}, {9559, 1}}, func(pid uint32) string { return names[pid] })
	if !reflect.DeepEqual(got, []int{9012, 9559}) {
		t.Fatalf("got %v", got)
	}
}
