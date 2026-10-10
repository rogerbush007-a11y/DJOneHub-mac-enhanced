package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSMSReadScanPreservesOutgoingAndIncomingStorage(t *testing.T) {
	// ME is full, while the incoming SM store still has space. Reading both
	// stores must never move incoming delivery back to the full ME store.
	memories := []string{"ME", "ME", "SM"}
	command := func(cmd string, _ time.Duration) (string, error) {
		fields, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(cmd, "AT+CPMS="))).Read()
		if err != nil || len(fields) > len(memories) {
			t.Fatalf("invalid storage selection %q", cmd)
		}
		copy(memories, fields)
		return "+CPMS: 1,10,23,23,1,10\r\nOK\r\n", nil
	}
	for _, memory := range []string{"SM", "ME", "SM", "ME"} {
		if _, err := selectSMSReadStorage(command, memory); err != nil {
			t.Fatal(err)
		}
		if memories[0] != memory || memories[1] != "ME" || memories[2] != "SM" {
			t.Fatalf("read scan changed outgoing/receive storage: %v", memories)
		}
	}
}

func TestSMSReadStorageRejectsModemError(t *testing.T) {
	for _, reply := range []string{"ERROR\r\n", "+CMS ERROR: 321\r\n", ""} {
		_, err := selectSMSReadStorage(func(string, time.Duration) (string, error) { return reply, nil }, "SM")
		if err == nil {
			t.Fatalf("reply %q was accepted", reply)
		}
	}
}

func TestParseSMSStorageStatus(t *testing.T) {
	status, err := parseSMSStorageStatus("AT+CPMS?\r\n+CPMS: \"ME\",23,23, \"ME\",23,23, \"SM\",1,10\r\nOK\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Read.Full() || !status.Write.Full() || status.Receive.Full() || status.Receive.Memory != "SM" || status.Receive.Total-status.Receive.Used != 9 {
		t.Fatalf("incorrect storage capacity: %#v", status)
	}
}

func TestParseSMSStorageStatusRejectsInvalidCapacity(t *testing.T) {
	for _, response := range []string{
		"ERROR",
		"+CPMS: 23,23,23,23,23,23",
		`+CPMS: "ME",-1,23,"ME",23,23,"SM",1,10`,
		`+CPMS: "ME",24,23,"ME",23,23,"SM",1,10`,
		`+CPMS: "ME",23,23,"ME",23,23,"SM",x,10`,
	} {
		if _, err := parseSMSStorageStatus(response); err == nil {
			t.Fatalf("invalid capacity accepted: %q", response)
		}
	}
}

func TestQuerySMSStorageDoesNotHideTransportFailure(t *testing.T) {
	want := errors.New("USB disconnected")
	status, err := querySMSStorage(func(string, time.Duration) (string, error) { return "", want })
	if status != nil || !errors.Is(err, want) {
		t.Fatalf("status=%v err=%v", status, err)
	}
}

func TestSMSStatusWarnsOnlyWhenIncomingStorageIsFull(t *testing.T) {
	for _, used := range []int{1, 10} {
		a := &app{smsStorage: &smsStorageStatus{
			Read:    smsStorageUsage{Memory: "ME", Used: 23, Total: 23},
			Receive: smsStorageUsage{Memory: "SM", Used: used, Total: 10},
		}}
		response := httptest.NewRecorder()
		a.smsStatus(response, httptest.NewRequest(http.MethodGet, "/api/sms/status", nil))
		var status struct {
			Storage *smsStorageStatus `json:"storage"`
			Warning string            `json:"storage_warning"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Storage == nil || (status.Warning != "") != (used == 10) {
			t.Fatalf("used=%d status=%#v", used, status)
		}
	}
}

func TestIncomingStorageRecoveryNeverDeletesOrChangesOutgoingStore(t *testing.T) {
	for _, simUsed := range []int{1, 10} {
		memories := []string{"ME", "ME", "ME"}
		capacity := map[string]smsStorageUsage{
			"ME": {Used: 23, Total: 23},
			"SM": {Used: simUsed, Total: 10},
		}
		command := func(cmd string, _ time.Duration) (string, error) {
			if cmd != "AT+CPMS?" {
				if !strings.HasPrefix(cmd, "AT+CPMS=") {
					t.Fatalf("unexpected or destructive command: %q", cmd)
				}
				fields, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(cmd, "AT+CPMS="))).Read()
				if err != nil || len(fields) > len(memories) {
					t.Fatalf("invalid storage selection: %q", cmd)
				}
				copy(memories, fields)
				return "+CPMS: 1,10,23,23,1,10\r\nOK\r\n", nil
			}
			var groups []string
			for _, memory := range memories {
				store := capacity[memory]
				groups = append(groups, fmt.Sprintf(`"%s",%d,%d`, memory, store.Used, store.Total))
			}
			return "+CPMS: " + strings.Join(groups, ",") + "\r\nOK\r\n", nil
		}
		status, err := ensureSMSReceiveStorage(command)
		if err != nil {
			t.Fatal(err)
		}
		wantReceive := "SM"
		if simUsed == 10 {
			wantReceive = "ME"
		}
		if memories[0] != "ME" || memories[1] != "ME" || memories[2] != wantReceive || status.Receive.Full() != (simUsed == 10) {
			t.Fatalf("simUsed=%d storage=%v status=%#v", simUsed, memories, status)
		}
		// A second poll must preserve the recovered receive store.
		if _, err := ensureSMSReceiveStorage(command); err != nil || memories[2] != wantReceive {
			t.Fatalf("second poll changed recovered receive store: %v, %v", memories, err)
		}
	}
}
