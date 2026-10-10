package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type smsStorageUsage struct {
	Memory string `json:"memory"`
	Used   int    `json:"used"`
	Total  int    `json:"total"`
}

func (s smsStorageUsage) Full() bool {
	return s.Total > 0 && s.Used >= s.Total
}

type smsStorageStatus struct {
	Read    smsStorageUsage `json:"read"`
	Write   smsStorageUsage `json:"write"`
	Receive smsStorageUsage `json:"receive"`
}

// CPMS mem1 selects the store for reading/deleting. Changing mem2 or mem3
// here would also change outgoing storage and where new messages arrive.
func selectSMSReadStorage(command func(string, time.Duration) (string, error), memory string) (string, error) {
	if memory != "SM" && memory != "ME" {
		return "", errors.New("unsupported SMS read storage")
	}
	response, err := command(fmt.Sprintf(`AT+CPMS="%s"`, memory), 5*time.Second)
	if err != nil {
		return response, err
	}
	if !atProbeSucceeded(response) {
		return response, errors.New("modem rejected SMS read storage selection")
	}
	return response, nil
}

func querySMSStorage(command func(string, time.Duration) (string, error)) (*smsStorageStatus, error) {
	response, err := command("AT+CPMS?", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("query SMS storage: %w", err)
	}
	if !atProbeSucceeded(response) {
		return nil, errors.New("modem rejected SMS storage query")
	}
	return parseSMSStorageStatus(response)
}

// A modem reset can restore ME as the incoming store. When that store is full,
// use a verified free store without deleting any messages or changing mem2.
func ensureSMSReceiveStorage(command func(string, time.Duration) (string, error)) (*smsStorageStatus, error) {
	status, err := querySMSStorage(command)
	if err != nil || !status.Receive.Full() {
		return status, err
	}
	original := *status
	for _, memory := range []string{"SM", "ME"} {
		if memory == original.Receive.Memory {
			continue
		}
		if _, err := selectSMSReadStorage(command, memory); err != nil {
			return status, err
		}
		candidate, err := querySMSStorage(command)
		if err != nil {
			return nil, err
		}
		if candidate.Read.Total == 0 || candidate.Read.Full() {
			continue
		}
		response, err := command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, original.Read.Memory, original.Write.Memory, memory), 5*time.Second)
		if err != nil {
			return nil, err
		}
		if !atProbeSucceeded(response) {
			return status, errors.New("modem rejected incoming SMS storage change")
		}
		updated, err := querySMSStorage(command)
		if err != nil {
			return nil, err
		}
		if updated.Receive.Memory != memory || updated.Receive.Full() {
			return updated, errors.New("incoming SMS storage change was not confirmed")
		}
		return updated, nil
	}
	if _, err := selectSMSReadStorage(command, original.Read.Memory); err != nil {
		return status, err
	}
	return querySMSStorage(command)
}

func parseSMSStorageStatus(response string) (*smsStorageStatus, error) {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CPMS:") {
			continue
		}
		reader := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CPMS:"))))
		reader.TrimLeadingSpace = true
		fields, err := reader.Read()
		if err != nil || len(fields) != 9 {
			return nil, errors.New("invalid SMS storage status")
		}
		status := &smsStorageStatus{}
		stores := []*smsStorageUsage{&status.Read, &status.Write, &status.Receive}
		for i, store := range stores {
			memory := strings.TrimSpace(fields[i*3])
			used, usedErr := strconv.Atoi(strings.TrimSpace(fields[i*3+1]))
			total, totalErr := strconv.Atoi(strings.TrimSpace(fields[i*3+2]))
			if memory == "" || usedErr != nil || totalErr != nil || used < 0 || total < 0 || used > total {
				return nil, errors.New("invalid SMS storage capacity")
			}
			*store = smsStorageUsage{Memory: memory, Used: used, Total: total}
		}
		return status, nil
	}
	return nil, errors.New("SMS storage status is missing")
}
