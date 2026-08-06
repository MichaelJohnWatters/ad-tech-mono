// helmparity normalizes two multi-document Kubernetes YAML streams and diffs
// them per-resource on the fields that matter. Used by scripts/helm-parity.sh
// to prove the helm chart (k8s/helm/adtech) matches the kustomize + Tilt
// manifests. See k8s/helm/adtech/PARITY.md for accepted residual diffs.
//
// Usage: helmparity <left.yaml> <right.yaml>
//
//	left  = kustomize/Tilt render (source of truth)
//	right = helm template render
//
// Exit codes: 0 = parity, 1 = material diffs, 2 = usage/parse error.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type doc = map[string]any

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: helmparity <left.yaml> <right.yaml>")
		os.Exit(2)
	}
	left, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", os.Args[1], err)
		os.Exit(2)
	}
	right, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", os.Args[2], err)
		os.Exit(2)
	}

	keys := map[string]bool{}
	for k := range left {
		keys[k] = true
	}
	for k := range right {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var pass, fail, missing, extra int
	for _, k := range sorted {
		l, lok := left[k]
		r, rok := right[k]
		switch {
		case lok && !rok:
			missing++
			fmt.Printf("MISSING %-60s (in kustomize/Tilt, not in helm)\n", k)
		case !lok && rok:
			extra++
			fmt.Printf("EXTRA   %-60s (in helm, not in kustomize/Tilt)\n", k)
		default:
			var diffs []string
			diff("", normalize(l), normalize(r), &diffs)
			if len(diffs) == 0 {
				pass++
				fmt.Printf("PASS    %s\n", k)
			} else {
				fail++
				fmt.Printf("DIFF    %s\n", k)
				for _, d := range diffs {
					fmt.Printf("        %s\n", d)
				}
			}
		}
	}

	fmt.Printf("\n%d PASS, %d DIFF, %d MISSING, %d EXTRA (of %d resources)\n",
		pass, fail, missing, extra, len(sorted))
	if fail+missing+extra > 0 {
		os.Exit(1)
	}
}

func load(path string) (map[string]doc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]doc{}
	dec := yaml.NewDecoder(f)
	for {
		var d doc
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if d == nil {
			continue
		}
		kind, _ := dig(d, "kind").(string)
		name, _ := dig(d, "metadata", "name").(string)
		ns, _ := dig(d, "metadata", "namespace").(string)
		if kind == "" || name == "" {
			continue
		}
		out[fmt.Sprintf("%s/%s/%s", kind, ns, name)] = d
	}
	return out, nil
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(doc)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// noiseLabels / noiseAnnotations are toolchain-managed metadata, cosmetic by
// definition — stripped from BOTH sides before comparing.
var noiseLabels = map[string]bool{
	"helm.sh/chart":                    true,
	"app.kubernetes.io/managed-by":     true,
	"app.kubernetes.io/instance":       true,
	"app.kubernetes.io/version":        true,
	"helm.toolkit.fluxcd.io/name":      true,
	"helm.toolkit.fluxcd.io/namespace": true,
}

func noisyAnnotation(k string) bool {
	return strings.HasPrefix(k, "meta.helm.sh/") ||
		strings.HasPrefix(k, "helm.sh/") ||
		strings.HasPrefix(k, "checksum/") ||
		k == "kubectl.kubernetes.io/last-applied-configuration"
}

// normalize deep-copies a resource with cosmetic noise removed and defaults
// filled so semantically-equal manifests compare equal.
func normalize(d doc) doc {
	out := clean(d).(doc)

	if md, ok := out["metadata"].(doc); ok {
		if labels, ok := md["labels"].(doc); ok {
			for k := range labels {
				if noiseLabels[k] {
					delete(labels, k)
				}
			}
			if len(labels) == 0 {
				delete(md, "labels")
			}
		}
		if ann, ok := md["annotations"].(doc); ok {
			for k := range ann {
				if noisyAnnotation(k) {
					delete(ann, k)
				}
			}
			if len(ann) == 0 {
				delete(md, "annotations")
			}
		}
		delete(md, "creationTimestamp")
	}
	delete(out, "status")

	kind, _ := out["kind"].(string)
	spec, _ := out["spec"].(doc)
	switch kind {
	case "Deployment", "StatefulSet":
		if spec != nil {
			if _, ok := spec["replicas"]; !ok {
				spec["replicas"] = 1
			}
			normalizePodTemplate(dig(spec, "template"))
		}
	case "DaemonSet":
		normalizePodTemplate(dig(spec, "template"))
	case "CronJob":
		normalizePodTemplate(dig(spec, "jobTemplate", "spec", "template"))
	case "Job":
		normalizePodTemplate(dig(spec, "template"))
	case "Service":
		if spec != nil {
			if _, ok := spec["type"]; !ok {
				spec["type"] = "ClusterIP"
			}
			if ports, ok := spec["ports"].([]any); ok {
				for _, p := range ports {
					if pm, ok := p.(doc); ok {
						if _, ok := pm["protocol"]; !ok {
							pm["protocol"] = "TCP"
						}
						// targetPort defaults to port
						if _, ok := pm["targetPort"]; !ok {
							pm["targetPort"] = pm["port"]
						}
					}
				}
				sortByField(ports, "port")
			}
		}
	}
	return out
}

func normalizePodTemplate(t any) {
	tm, ok := t.(doc)
	if !ok {
		return
	}
	podSpec, ok := tm["spec"].(doc)
	if !ok {
		return
	}
	for _, field := range []string{"containers", "initContainers"} {
		cs, _ := podSpec[field].([]any)
		for _, c := range cs {
			cm, ok := c.(doc)
			if !ok {
				continue
			}
			if env, ok := cm["env"].([]any); ok {
				sortByField(env, "name")
			}
			if ports, ok := cm["ports"].([]any); ok {
				for _, p := range ports {
					if pm, ok := p.(doc); ok {
						if _, ok := pm["protocol"]; !ok {
							pm["protocol"] = "TCP"
						}
					}
				}
				sortByField(ports, "containerPort")
			}
			if vm, ok := cm["volumeMounts"].([]any); ok {
				sortByField(vm, "name")
			}
		}
	}
	if vols, ok := podSpec["volumes"].([]any); ok {
		sortByField(vols, "name")
	}
}

func sortByField(list []any, field string) {
	sort.SliceStable(list, func(i, j int) bool {
		fi := fmt.Sprintf("%v", dig(list[i], field))
		fj := fmt.Sprintf("%v", dig(list[j], field))
		return fi < fj
	})
}

// clean deep-copies, dropping nil values and empty maps.
func clean(v any) any {
	switch t := v.(type) {
	case doc:
		out := doc{}
		for k, val := range t {
			if val == nil {
				continue
			}
			out[k] = clean(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, clean(val))
		}
		return out
	default:
		return v
	}
}

func diff(path string, l, r any, out *[]string) {
	switch lt := l.(type) {
	case doc:
		rt, ok := r.(doc)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: type mismatch (%s vs %s)", path, short(l), short(r)))
			return
		}
		keys := map[string]bool{}
		for k := range lt {
			keys[k] = true
		}
		for k := range rt {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			lv, lok := lt[k]
			rv, rok := rt[k]
			p := path + "." + k
			switch {
			case lok && !rok:
				*out = append(*out, fmt.Sprintf("%s: only in kustomize/Tilt: %s", p, short(lv)))
			case !lok && rok:
				*out = append(*out, fmt.Sprintf("%s: only in helm: %s", p, short(rv)))
			default:
				diff(p, lv, rv, out)
			}
		}
	case []any:
		rt, ok := r.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: type mismatch (%s vs %s)", path, short(l), short(r)))
			return
		}
		if len(lt) != len(rt) {
			*out = append(*out, fmt.Sprintf("%s: list length %d vs %d", path, len(lt), len(rt)))
			return
		}
		for i := range lt {
			diff(fmt.Sprintf("%s[%d]", path, i), lt[i], rt[i], out)
		}
	default:
		// Scalars: compare by canonical string+type. Note int(1) vs "1" must
		// NOT be equal (quoted resource quantities matter), so include type.
		if fmt.Sprintf("%T:%v", l, l) != fmt.Sprintf("%T:%v", r, r) {
			*out = append(*out, fmt.Sprintf("%s: %s != %s", path, short(l), short(r)))
		}
	}
}

func short(v any) string {
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return fmt.Sprintf("%q(%T)", s, v)
}
