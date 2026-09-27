package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

const modelScanMaxBytes = 64 << 10

// Keep framing state per request; network chunk boundaries are not SSE events.
type modelSSEDecoder struct {
	line   []byte
	data   []byte
	event  string
	total  int
	skipLF bool
	done   bool
}

func (d *modelSSEDecoder) feed(chunk []byte) (string, bool) {
	if d.done || len(chunk) > modelScanMaxBytes-d.total {
		d.done = true
		return "", false
	}
	d.total += len(chunk)
	for _, b := range chunk {
		if d.skipLF {
			d.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b != '\r' && b != '\n' {
			d.line = append(d.line, b)
			continue
		}
		d.skipLF = b == '\r'
		model, ok := d.consumeLine()
		d.line = d.line[:0]
		if ok || d.done {
			return model, ok
		}
	}
	return "", false
}

func (d *modelSSEDecoder) consumeLine() (string, bool) {
	if len(d.line) != 0 {
		field, value, _ := bytes.Cut(d.line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			d.data = append(d.data, value...)
			d.data = append(d.data, '\n')
		case "event":
			d.event = string(value)
		}
		return "", false
	}
	data, event := bytes.TrimSpace(d.data), d.event
	d.data, d.event = nil, ""
	if len(data) == 0 {
		return "", false
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		d.done = true
		return "", false
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil {
		d.done = true
		return "", false
	}
	if ev.Type != "response.created" {
		if ev.Type == "response.completed" || ev.Type == "response.failed" || ev.Type == "response.incomplete" || ev.Type == "error" {
			d.done = true
		}
		return "", false
	}
	d.done = true
	if (event != "" && event != "response.created") || strings.TrimSpace(ev.Response.ID) == "" || strings.TrimSpace(ev.Response.Model) == "" {
		return "", false
	}
	return ev.Response.Model, true
}

func consumeModelScanChunk(requestID string, chunk []byte) (pendingModelScanEntry, string, bool) {
	pendingModelScans.mu.Lock()
	defer pendingModelScans.mu.Unlock()
	entry, ok := pendingModelScans.byID[requestID]
	if !ok {
		return pendingModelScanEntry{}, "", false
	}
	if time.Since(entry.seenAt) > pendingAuthTTL {
		delete(pendingModelScans.byID, requestID)
		return pendingModelScanEntry{}, "", false
	}
	if entry.decoder == nil {
		entry.decoder = &modelSSEDecoder{}
	}
	model, found := entry.decoder.feed(chunk)
	if entry.decoder.done {
		delete(pendingModelScans.byID, requestID)
	} else {
		pendingModelScans.byID[requestID] = entry
	}
	return entry, model, found
}
