// Package api holds the JSON types of the taraengine HTTP API, shared by the server, the client and
// taracode.
package api

// Memory is what a loaded model holds: measured on the GPU, or estimated from resident memory.
type Memory struct {
	Bytes  int64  `json:"bytes"`
	Source string `json:"source"`
}

// Model is one catalog or pulled model.
type Model struct {
	Name                 string   `json:"name"`
	Status               string   `json:"status"`
	Size                 int64    `json:"size"`
	Family               string   `json:"family,omitempty"`
	Params               string   `json:"params,omitempty"`
	Quantization         string   `json:"quantization,omitempty"`
	Context              int      `json:"context"`
	NativeContext        int      `json:"native_context,omitempty"`
	Capabilities         []string `json:"capabilities"`
	CapabilitiesInferred bool     `json:"capabilities_inferred"`
	Tier                 string   `json:"tier,omitempty"`
	Default              bool     `json:"default"`
	Memory               *Memory  `json:"memory,omitempty"`
	ExitCode             int      `json:"exit_code,omitempty"`
	Error                string   `json:"error,omitempty"`
}

// ModelDetail is the show call.
type ModelDetail struct {
	Model
	Digest  string            `json:"digest,omitempty"`
	Preset  map[string]string `json:"preset"`
	LogTail string            `json:"log_tail,omitempty"`
}

// Loaded is one resident model. Status is loaded or sleeping; a daemon before 0.2.0 leaves it out.
type Loaded struct {
	Name    string  `json:"name"`
	Context int     `json:"context"`
	Memory  *Memory `json:"memory,omitempty"`
	Status  string  `json:"status,omitempty"`
}

// RuntimeInfo names the backend and its build.
type RuntimeInfo struct {
	Build   string `json:"build"`
	Backend string `json:"backend"`
}

// Version is GET /api/version.
type Version struct {
	Version  string      `json:"version"`
	Runtime  RuntimeInfo `json:"runtime"`
	Platform string      `json:"platform"`
}

// Health is GET /api/health.
type Health struct {
	Status  string `json:"status"`
	Backend string `json:"backend"`
	Detail  string `json:"detail,omitempty"`
}

// LoadRequest is POST /api/load.
type LoadRequest struct {
	Model   string `json:"model"`
	Context int    `json:"context,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// UnloadRequest is POST /api/unload.
type UnloadRequest struct {
	Model string `json:"model"`
}

// PullRequest is POST /api/pull; Model is a catalog name, hf://owner/repo/file.gguf or an absolute path.
type PullRequest struct {
	Model string `json:"model"`
}

// PullEvent is one progress event of a pull.
type PullEvent struct {
	Stage string `json:"stage"`
	File  string `json:"file,omitempty"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	Error string `json:"error,omitempty"`
}

// Machine describes the host.
type Machine struct {
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	GPU            string `json:"gpu"`
	GPUMemoryBytes int64  `json:"gpu_memory_bytes"`
	UnifiedMemory  bool   `json:"unified_memory"`
	RAMBytes       int64  `json:"ram_bytes"`
	Tier           string `json:"tier"`
	DriverVersion  string `json:"driver_version,omitempty"`
}

// RuntimeStatus describes the pinned build on this machine.
type RuntimeStatus struct {
	Installed bool   `json:"installed"`
	Verified  bool   `json:"verified"`
	Build     string `json:"build"`
	Dir       string `json:"dir"`
	Error     string `json:"error,omitempty"`
}

// Doctor is GET /api/doctor.
type Doctor struct {
	Machine        Machine           `json:"machine"`
	Runtime        RuntimeStatus     `json:"runtime"`
	Recommendation map[string]string `json:"recommendation"`
}

// ErrorBody is the error payload.
type ErrorBody struct {
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Error is every error answer: {"error":{"message":"...","hint":"..."}}.
type Error struct {
	Error ErrorBody `json:"error"`
}
