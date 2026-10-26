package adapter

import (
	"os"
	"path/filepath"
	"strings"
)

// WorkdirOption is the shared option spec entry for adapters that spawn a
// process. Keep the key identical across adapters so the wizard and the docs
// only have to explain it once.
var WorkdirOption = Option{
	Key:     "workdir",
	Label:   "Working directory (blank = a fresh temp dir per request)",
	Default: "",
}

// workdir decides where a spawned process should run.
//
// These CLIs are autonomous agents that can read and write files. Running them
// in whatever directory the server happened to start in means a request can
// touch the operator's source tree, and two concurrent requests share state.
// So the default is a throwaway directory per request, and pointing at a real
// project is an explicit opt-in.
//
// The returned cleanup is always safe to call and only removes directories
// this function created.
func workdir(opts map[string]string) (dir string, cleanup func(), err error) {
	configured := strings.TrimSpace(opts["workdir"])
	if configured != "" {
		if strings.HasPrefix(configured, "~") {
			if home, herr := os.UserHomeDir(); herr == nil {
				configured = filepath.Join(home, strings.TrimPrefix(configured, "~"))
			}
		}
		return configured, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "cli-to-api-*")
	if err != nil {
		return "", func() {}, err
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}
