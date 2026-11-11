package imagebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// StackLayer is supplied in dependency order by the existing workspace graph
// projection. Native OS/toolchain/dependency layers precede package layers.
// Digest is the actual published payload SHA256, never a guessed version.
type StackLayer struct {
	Name         string   `json:"name"`
	Digest       string   `json:"digest"`
	Dependencies []string `json:"dependencies"`
	Script       string   `json:"script"`
	ScriptDigest string   `json:"scriptDigest"`
}

// StackIdentities hashes exact ordered prefixes, including base and recipes.
// It consumes order; it neither discovers nor sorts a second dependency graph.
func StackIdentities(base string, layers []StackLayer) ([]string, error) {
	if base == "" || len(layers) == 0 {
		return nil, fmt.Errorf("base identity and nonempty ordered layers required")
	}
	names := map[string]int{}
	for i, l := range layers {
		if l.Name == "" {
			return nil, fmt.Errorf("layer name required")
		}
		if _, ok := names[l.Name]; ok {
			return nil, fmt.Errorf("duplicate layer %s", l.Name)
		}
		names[l.Name] = i
	}
	var result []string
	parent := base
	for i, l := range layers {
		for _, d := range []string{strings.TrimPrefix(l.Digest, "sha256:"), l.ScriptDigest} {
			b, err := hex.DecodeString(d)
			if err != nil || len(b) != sha256.Size {
				return nil, fmt.Errorf("layer %s requires actual payload and recipe SHA256", l.Name)
			}
		}
		for _, dep := range l.Dependencies {
			if j, selected := names[dep]; selected && j >= i {
				return nil, fmt.Errorf("layer %s precedes dependency %s", l.Name, dep)
			}
		}
		b, _ := json.Marshal(struct{ Parent, Name, Payload, Recipe string }{parent, l.Name, l.Digest, l.ScriptDigest})
		sum := sha256.Sum256(b)
		parent = hex.EncodeToString(sum[:])
		result = append(result, parent)
	}
	return result, nil
}
