package ref

import (
	"bytes"
	"os"
	"path"
	"strings"

	"encoding/json"
	"k8s.io/apimachinery/pkg/runtime"
)

type Config struct {
	Path string
}

type KubeConfig struct {
	Path string
}

func (kr *KubeConfig) Write(data []byte) error {
	if err := os.MkdirAll(path.Dir(kr.Path), 0700); err != nil {
		return err
	}
	m := string(data)
	m = strings.TrimSpace(string(data))
	m = strings.ReplaceAll(m, "\t", "  ")
	if !strings.HasSuffix(m, "\n") {
		m = m + "\n"
	}

	return os.WriteFile(kr.Path, data, 0700)
}

func (cr *Config) Write(data []byte) error {
	if err := os.MkdirAll(path.Dir(cr.Path), 0700); err != nil {
		return err
	}
	m := string(data)
	m = strings.TrimSpace(string(data))
	m = strings.ReplaceAll(m, "\t", "  ")
	if !strings.HasSuffix(m, "\n") {
		m = m + "\n"
	}
	return os.WriteFile(cr.Path, []byte(m), 0700)
}

func (cr *Config) WriteObject(obj runtime.Object) error {
	if err := os.MkdirAll(path.Dir(cr.Path), 0700); err != nil {
		return err
	}

	w := &bytes.Buffer{}
	ye := json.NewEncoder(w)
	ye.SetIndent("  ", "  ")
	if err := ye.Encode(obj); err != nil {
		return err
	}

	return os.WriteFile(cr.Path, w.Bytes(), 0700)
}
