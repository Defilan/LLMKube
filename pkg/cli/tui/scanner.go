/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LocalModel represents a model file (or model directory) discovered on the
// user's local disk. The TUI merges these against the curated catalog so the
// browser can show what's already downloaded vs what would need to be fetched.
type LocalModel struct {
	// Path is the absolute filesystem path. For GGUF this is the .gguf file
	// itself; for safetensors / MLX / PyTorch layouts this is the containing
	// directory (since those formats span multiple files).
	Path string
	// Format is one of "gguf", "safetensors", "mlx", "pytorch".
	Format string
	// SizeBytes is the on-disk footprint. For directory-rooted formats it is
	// the recursive sum of regular files within the dir.
	SizeBytes int64
	// Quant, when parsable from the filename, is one of "Q4_K_M", "Q8_0",
	// "8bit", "4bit", "FP16", etc. Empty when not detectable.
	Quant string
	// SourceDir is the scan root that found this model (e.g. "~/llmkube-models").
	// Used in the TUI to group results.
	SourceDir string
	// DisplayName is a humanized identifier for the row. For GGUF it's the
	// filename minus extension; for directory layouts it's the dir basename.
	DisplayName string
}

// ScanPaths returns the absolute paths the scanner walks. Paths that don't
// exist are skipped silently at scan time.
//
// Order is intentional: dirs that are most likely to contain LLMKube-deployed
// models come first so the browser surfaces the user's primary working set
// before generic HF cache.
func ScanPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, "llmkube-models"),               // LLMKube model store convention
		filepath.Join(home, "models"),                       // vllm-swift download convention
		filepath.Join(home, ".lmstudio", "models"),          // LM Studio cache (common on Macs)
		filepath.Join(home, ".cache", "huggingface", "hub"), // HF transformers cache
	}
}

// ScanLocal walks each path in ScanPaths(), classifies the contents by format,
// and returns the union as a sorted slice. Best-effort: a missing or
// permission-denied path is skipped without erroring. Returns nil only when
// the home dir itself can't be resolved.
func ScanLocal() []LocalModel {
	var results []LocalModel
	for _, root := range ScanPaths() {
		if !dirExists(root) {
			continue
		}
		results = append(results, scanRoot(root)...)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].SourceDir != results[j].SourceDir {
			return results[i].SourceDir < results[j].SourceDir
		}
		return results[i].DisplayName < results[j].DisplayName
	})
	return results
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// scanRoot walks one scan root one level deep. The HF cache exception walks
// the standard `models--<org>--<repo>/snapshots/<sha>` shape, which is
// effectively three levels deep but produces one entry per repo/snapshot.
func scanRoot(root string) []LocalModel {
	if filepath.Base(root) == "hub" && strings.Contains(root, "huggingface") {
		return scanHFCache(root)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []LocalModel
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		// Use Stat (follows symlinks) so HF-cache-style symlinked dirs and
		// user-created symlinks both resolve correctly.
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			out = append(out, classifyDir(full, root)...)
		} else if info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(entry.Name()), ".gguf") {
			out = append(out, ggufFromFile(full, root))
		}
	}
	return out
}

// scanHFCache walks ~/.cache/huggingface/hub. The HF cache layout is:
//
//	hub/
//	  models--<org>--<repo>/
//	    snapshots/<sha>/<files>
//
// We pick the most-recent snapshot per repo and classify it by the files
// it contains. Skips repos with no snapshots.
func scanHFCache(hubRoot string) []LocalModel {
	entries, err := os.ReadDir(hubRoot)
	if err != nil {
		return nil
	}
	var out []LocalModel
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "models--") {
			continue
		}
		// Follow symlinks: HF cache uses blob+symlink dedup and tests may
		// also stage repos via symlink.
		repoPath := filepath.Join(hubRoot, entry.Name())
		info, err := os.Stat(repoPath)
		if err != nil || !info.IsDir() {
			continue
		}
		snapshotsDir := filepath.Join(hubRoot, entry.Name(), "snapshots")
		snaps, err := os.ReadDir(snapshotsDir)
		if err != nil || len(snaps) == 0 {
			continue
		}
		// Pick the newest snapshot by modtime so we don't dedupe-spam the UI
		// when a repo has been updated through multiple revisions.
		var newest os.DirEntry
		var newestPath string
		for _, snap := range snaps {
			candidate := filepath.Join(snapshotsDir, snap.Name())
			if newest == nil {
				newest, newestPath = snap, candidate
				continue
			}
			si, _ := snap.Info()
			ni, _ := newest.Info()
			if si != nil && ni != nil && si.ModTime().After(ni.ModTime()) {
				newest, newestPath = snap, candidate
			}
		}
		if newest == nil {
			continue
		}
		// Convert "models--meta-llama--Llama-3.1-8B" → "meta-llama/Llama-3.1-8B"
		display := strings.TrimPrefix(entry.Name(), "models--")
		display = strings.Replace(display, "--", "/", 1)
		out = append(out, classifyDirWithName(newestPath, hubRoot, display)...)
	}
	return out
}

// classifyDir inspects a directory and emits zero, one, or many LocalModel
// records. A directory may contain both a single GGUF (one record) or be a
// multi-file format root like safetensors (one record for the whole dir).
func classifyDir(dir, sourceDir string) []LocalModel {
	return classifyDirWithName(dir, sourceDir, filepath.Base(dir))
}

func classifyDirWithName(dir, sourceDir, displayName string) []LocalModel {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var (
		ggufFiles      []string
		hasSafetensors bool
		hasMLX         bool
		hasPytorch     bool
		hasConfigJSON  bool
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.EqualFold(filepath.Ext(name), ".gguf"):
			ggufFiles = append(ggufFiles, filepath.Join(dir, name))
		case strings.EqualFold(filepath.Ext(name), ".safetensors"),
			name == "model.safetensors.index.json":
			hasSafetensors = true
		case name == "mlx_lm.json", strings.HasSuffix(name, ".npz"):
			hasMLX = true
		case strings.HasPrefix(name, "pytorch_model") && strings.HasSuffix(name, ".bin"):
			hasPytorch = true
		case name == "config.json":
			hasConfigJSON = true
		}
	}

	var out []LocalModel
	// Each GGUF file becomes its own row. This is the right shape because
	// multiple quants of the same model commonly coexist (Q4_K_M.gguf and
	// Q8_0.gguf both in the same dir) and the user picks one per deploy.
	for _, gp := range ggufFiles {
		out = append(out, ggufFromFile(gp, sourceDir))
	}

	// Treat MLX layouts (often dir name contains -MLX or -bit) as a single
	// LocalModel. MLX detection prefers explicit signals (mlx_lm.json, .npz)
	// but also catches mlx-community/* dirs by name.
	if hasMLX || mlxNameHint(displayName) {
		out = append(out, LocalModel{
			Path:        dir,
			Format:      "mlx",
			SizeBytes:   dirSize(dir),
			Quant:       parseQuantFromName(displayName),
			SourceDir:   sourceDir,
			DisplayName: displayName,
		})
		return out
	}

	if hasSafetensors && hasConfigJSON {
		out = append(out, LocalModel{
			Path:        dir,
			Format:      "safetensors",
			SizeBytes:   dirSize(dir),
			Quant:       parseQuantFromName(displayName),
			SourceDir:   sourceDir,
			DisplayName: displayName,
		})
	} else if hasPytorch && hasConfigJSON {
		out = append(out, LocalModel{
			Path:        dir,
			Format:      "pytorch",
			SizeBytes:   dirSize(dir),
			Quant:       parseQuantFromName(displayName),
			SourceDir:   sourceDir,
			DisplayName: displayName,
		})
	}
	return out
}

// ggufFromFile builds a LocalModel for a single .gguf file. Quant is parsed
// from the filename via parseQuantFromName, which handles bartowski-style
// naming (Llama-3.1-8B-Instruct-Q5_K_M.gguf) and the common variants.
func ggufFromFile(path, sourceDir string) LocalModel {
	st, err := os.Stat(path)
	var size int64
	if err == nil {
		size = st.Size()
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return LocalModel{
		Path:        path,
		Format:      "gguf",
		SizeBytes:   size,
		Quant:       parseQuantFromName(name),
		SourceDir:   sourceDir,
		DisplayName: name,
	}
}

// dirSize sums the size of regular files under root, recursively.
// Symlinks are not followed (we don't want to double-count or chase loops in
// HF cache, which uses blob+symlink dedup).
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees silently
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// quantPattern matches GGUF and bit-style quant suffixes in any case:
//
//	-Q4_K_M, -Q5_K_M, -Q8_0, -IQ4_NL, -8bit, -4bit, -FP16, -BF16
//
// Order matters in the alternation: IQ\d... must come before Q\d... so the
// longer prefix wins ("IQ4_NL" doesn't get truncated to "Q4_NL").
var quantPattern = regexp.MustCompile(`(?i)[-_](IQ\d+_?[a-z0-9_]*|Q\d+_?[a-z0-9_]*|FP16|BF16|F16|F32|\d+bit)`)

// parseQuantFromName extracts a recognized quantization suffix from a model
// filename or directory name. Empty when no match. The returned token is
// uppercased to match the convention used in the catalog (Q5_K_M, etc.).
func parseQuantFromName(name string) string {
	match := quantPattern.FindStringSubmatch(name)
	if len(match) < 2 {
		return ""
	}
	return strings.ToUpper(match[1])
}

// mlxNameHint catches mlx-community/* style names where the directory has
// no mlx_lm.json present (because the user only downloaded weights, not
// runtime metadata).
func mlxNameHint(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "-mlx") || strings.Contains(lower, "mlx-")
}
