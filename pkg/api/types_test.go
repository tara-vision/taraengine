package api

import (
	"encoding/json"
	"testing"
)

// The wire names are the contract between the daemon, the typed client and taracode, so each one is pinned
// here by a literal: a renamed tag or a changed omitempty fails the test instead of a distant client.
func TestWireFormat(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			"Model with every field",
			Model{
				Name: "gemma4:12b", Status: "loaded", Size: 7, Family: "gemma4", Params: "12B", Quantization: "Q4_0",
				Context: 32768, NativeContext: 262144, Capabilities: []string{"tools", "vision"}, CapabilitiesInferred: true,
				Tier: "16", Default: true, Memory: &Memory{Bytes: 9, Source: "measured"}, ExitCode: 137, Error: "oom",
			},
			`{"name":"gemma4:12b","status":"loaded","size":7,"family":"gemma4","params":"12B","quantization":"Q4_0",` +
				`"context":32768,"native_context":262144,"capabilities":["tools","vision"],"capabilities_inferred":true,` +
				`"tier":"16","default":true,"origin":"","memory":{"bytes":9,"source":"measured"},"exit_code":137,"error":"oom"}`,
		},
		{
			"Model keeps the empty capability list and drops the optional fields",
			Model{Name: "x", Status: "available", Capabilities: []string{}},
			`{"name":"x","status":"available","size":0,"context":0,"capabilities":[],"capabilities_inferred":false,"default":false,"origin":""}`,
		},
		{
			"ModelDetail flattens the model",
			ModelDetail{
				Model:  Model{Name: "x", Status: "pulled", Capabilities: []string{}},
				Digest: "3712b9bd32ca", Preset: map[string]string{"c": "8192"}, LogTail: "line",
			},
			`{"name":"x","status":"pulled","size":0,"context":0,"capabilities":[],"capabilities_inferred":false,` +
				`"default":false,"origin":"","digest":"3712b9bd32ca","preset":{"c":"8192"},"log_tail":"line"}`,
		},
		{
			"ModelDetail without digest or log",
			ModelDetail{Model: Model{Name: "x", Status: "pulled", Capabilities: []string{}}, Preset: map[string]string{}},
			`{"name":"x","status":"pulled","size":0,"context":0,"capabilities":[],"capabilities_inferred":false,` +
				`"default":false,"origin":"","preset":{}}`,
		},
		{
			"Loaded",
			Loaded{Name: "x", Context: 8192, Memory: &Memory{Bytes: 1, Source: "estimated"}},
			`{"name":"x","context":8192,"memory":{"bytes":1,"source":"estimated"}}`,
		},
		{"Loaded without memory", Loaded{Name: "x", Context: 8192}, `{"name":"x","context":8192}`},
		{
			"Loaded asleep",
			Loaded{Name: "x", Context: 8192, Memory: &Memory{Bytes: 1, Source: "measured"}, Status: "sleeping"},
			`{"name":"x","context":8192,"memory":{"bytes":1,"source":"measured"},"status":"sleeping"}`,
		},
		{
			"Version",
			Version{Version: "0.1.0", Runtime: RuntimeInfo{Build: "b11514", Backend: "llama.cpp"}, Platform: "linux/amd64/cuda12"},
			`{"version":"0.1.0","runtime":{"build":"b11514","backend":"llama.cpp"},"platform":"linux/amd64/cuda12"}`,
		},
		{"Health", Health{Status: "degraded", Backend: "llama.cpp", Detail: "gone"}, `{"status":"degraded","backend":"llama.cpp","detail":"gone"}`},
		{"Health without detail", Health{Status: "ok", Backend: "llama.cpp"}, `{"status":"ok","backend":"llama.cpp"}`},
		{
			"LoadRequest",
			LoadRequest{Model: "x", Context: 4096, Force: true},
			`{"model":"x","context":4096,"force":true}`,
		},
		{"LoadRequest with defaults", LoadRequest{Model: "x"}, `{"model":"x"}`},
		{"UnloadRequest", UnloadRequest{Model: "x"}, `{"model":"x"}`},
		{"PullRequest", PullRequest{Model: "hf://o/r/f.gguf"}, `{"model":"hf://o/r/f.gguf"}`},
		{
			"PullEvent",
			PullEvent{Stage: "downloading", File: "f.gguf", Bytes: 5, Total: 10},
			`{"stage":"downloading","file":"f.gguf","bytes":5,"total":10}`,
		},
		{"PullEvent error", PullEvent{Stage: "error", Error: "boom"}, `{"stage":"error","bytes":0,"total":0,"error":"boom"}`},
		{
			"Doctor",
			Doctor{
				Machine: Machine{
					OS: "linux", Arch: "amd64", GPU: "RTX", GPUMemoryBytes: 1, UnifiedMemory: true, RAMBytes: 2, Tier: "48",
					DriverVersion: "570",
				},
				Runtime:        RuntimeStatus{Installed: true, Verified: true, Build: "b1", Dir: "/rt", Error: "bad"},
				Recommendation: map[string]string{"48": "gemma4:31b"},
			},
			`{"machine":{"os":"linux","arch":"amd64","gpu":"RTX","gpu_memory_bytes":1,"unified_memory":true,` +
				`"ram_bytes":2,"tier":"48","driver_version":"570"},"runtime":{"installed":true,"verified":true,` +
				`"build":"b1","dir":"/rt","error":"bad"},"recommendation":{"48":"gemma4:31b"}}`,
		},
		{
			"Doctor with optional fields empty",
			Doctor{Recommendation: map[string]string{}},
			`{"machine":{"os":"","arch":"","gpu":"","gpu_memory_bytes":0,"unified_memory":false,"ram_bytes":0,"tier":""},` +
				`"runtime":{"installed":false,"verified":false,"build":"","dir":""},"recommendation":{}}`,
		},
		{
			"Error",
			Error{Error: ErrorBody{Message: "m", Hint: "h"}},
			`{"error":{"message":"m","hint":"h"}}`,
		},
		{"Error without hint", Error{Error: ErrorBody{Message: "m"}}, `{"error":{"message":"m"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire form changed\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// What a client sends decodes into the request types, and what the server wrote decodes back unchanged.
func TestWireRoundTrip(t *testing.T) {
	in := ModelDetail{
		Model: Model{
			Name: "gemma4:12b", Status: "loaded", Size: 7, Context: 8192, Capabilities: []string{"tools"},
			Memory: &Memory{Bytes: 9, Source: "measured"},
		},
		Digest: "3712b9bd32ca", Preset: map[string]string{"c": "8192"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ModelDetail
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Memory == nil || out.Memory.Bytes != 9 || out.Preset["c"] != "8192" || out.Digest != in.Digest {
		t.Fatalf("round trip lost data: %+v", out)
	}
	var req LoadRequest
	if err := json.Unmarshal([]byte(`{"model":"x","context":4096,"force":true}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "x" || req.Context != 4096 || !req.Force {
		t.Fatalf("load request: %+v", req)
	}
}
