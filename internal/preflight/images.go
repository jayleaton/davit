package preflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/guestclient"
)

// imageManifest is the part of an images/ manifest the check reads.
type imageManifest struct {
	Name   string     `json:"name"`
	Kernel imageBlob  `json:"kernel"`
	Rootfs imageBlob  `json:"rootfs"`
	Guest  imageGuest `json:"guest"`
}

type imageBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type imageGuest struct {
	Agent struct {
		Binary *struct {
			Digest string `json:"digest"`
		} `json:"binary"`
		Version  string                     `json:"version"`
		Protocol *guestclient.AgentProtocol `json:"protocol"`
	} `json:"agent"`
}

// ImageFacts is what the check found for one image source.
type ImageFacts struct {
	Manifest string
	Name     string
	Err      error // unreadable manifest or missing/mis-sized blob
	Agent    bool  // the manifest records a baked-in guest agent
	// Compat grades the baked agent's protocol against this host (only when
	// Agent); CompatMsg explains anything but CompatOK.
	Compat    guestclient.Compat
	CompatMsg string
}

// inspectImage parses a manifest and checks its blobs exist with the right
// size (the daemon verifies digests at start; that costs seconds per GiB).
func inspectImage(src config.ImageSource) ImageFacts {
	f := ImageFacts{Manifest: src.Manifest}
	raw, err := os.ReadFile(src.Manifest)
	if err != nil {
		f.Err = err
		return f
	}
	var m imageManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		f.Err = fmt.Errorf("parse %s: %w", src.Manifest, err)
		return f
	}
	f.Name = m.Name
	f.Agent = m.Guest.Agent.Binary != nil && m.Guest.Agent.Binary.Digest != ""
	if f.Agent {
		f.Compat, f.CompatMsg = guestclient.CheckAgent(m.Guest.Agent.Protocol)
	}
	for what, b := range map[string]imageBlob{"kernel": m.Kernel, "rootfs": m.Rootfs} {
		hexd, ok := strings.CutPrefix(b.Digest, "sha256:")
		if !ok {
			f.Err = fmt.Errorf("%s: bad %s digest %q", m.Name, what, b.Digest)
			return f
		}
		fi, err := os.Stat(filepath.Join(src.BlobDir, "sha256", hexd))
		switch {
		case err != nil:
			f.Err = fmt.Errorf("%s: %s blob missing in %s", m.Name, what, src.BlobDir)
			return f
		case fi.Size() != b.Size:
			f.Err = fmt.Errorf("%s: %s blob is %d bytes, manifest says %d", m.Name, what, fi.Size(), b.Size)
			return f
		}
	}
	return f
}

// ClassifyImages grades the image set for a runtime that boots real VMs.
func ClassifyImages(facts []ImageFacts, required bool) Check {
	c := Check{ID: "images", Title: "Guest images"}
	hint := buildinfo.Name + " init --image <manifest.json> [--image-blobs <dir>]"
	if len(facts) == 0 {
		c.Status, c.Detail, c.Hint = severity(required), "no guest image configured: the daemon cannot boot VMs", hint
		return c
	}
	var ok, noAgent, bad, skew []string
	for _, f := range facts {
		switch {
		case f.Err != nil:
			bad = append(bad, f.Err.Error())
		case f.Compat == guestclient.CompatIncompatible:
			bad = append(bad, f.Name+": "+f.CompatMsg)
		case f.Compat == guestclient.CompatDegraded:
			skew = append(skew, f.Name+": "+f.CompatMsg)
		case !f.Agent:
			noAgent = append(noAgent, f.Name)
		default:
			ok = append(ok, f.Name)
		}
	}
	switch {
	case len(bad) > 0:
		c.Status, c.Detail, c.Hint = Fail, strings.Join(bad, "; "), "rebuild or re-download the image, then "+hint
	case len(skew) > 0:
		c.Status, c.Detail = Warn, strings.Join(skew, "; ")
		c.Hint = "rebuild the images with this " + buildinfo.Name + " version (images/build.sh), then " + hint
	case len(ok) == 0:
		c.Status = Warn
		c.Detail = fmt.Sprintf("%s records no guest agent: VMs never become ready unless one was added after the build", strings.Join(noAgent, ", "))
		c.Hint = "rebuild the image with images/build.sh, which bakes in this repository's guest agent"
	default:
		c.Status = Pass
		c.Detail = strings.Join(ok, ", ") + " (blobs present, guest agent baked in)"
		if len(noAgent) > 0 {
			c.Detail += "; without agent: " + strings.Join(noAgent, ", ")
		}
	}
	return c
}

func imagesCheck(o Options) Check {
	if !o.needsVM() {
		return Check{ID: "images", Title: "Guest images", Status: Skip, Detail: fmt.Sprintf("runtime is %q", o.Runtime)}
	}
	srcs := slices.Clone(o.Images)
	if len(srcs) == 0 {
		dir := filepath.Join(o.DataDir, "images")
		found, _ := filepath.Glob(filepath.Join(dir, "manifests", "*.json"))
		for _, m := range found {
			srcs = append(srcs, config.ImageSource{Manifest: m, BlobDir: filepath.Join(dir, "blobs")})
		}
	}
	var facts []ImageFacts
	for _, s := range srcs {
		facts = append(facts, inspectImage(s))
	}
	return ClassifyImages(facts, true)
}
