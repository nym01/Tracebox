package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepOrphanedJails_RemovesOldDirs(t *testing.T) {
	base := t.TempDir()

	old := filepath.Join(base, "goboxd-old123")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	sweepOrphanedJails(base, "goboxd-", 10*time.Minute)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expected old orphaned directory to be removed")
	}
}

func TestSweepOrphanedJails_KeepsRecentDirs(t *testing.T) {
	base := t.TempDir()

	recent := filepath.Join(base, "goboxd-recent456")
	if err := os.Mkdir(recent, 0700); err != nil {
		t.Fatal(err)
	}

	sweepOrphanedJails(base, "goboxd-", 10*time.Minute)

	if _, err := os.Stat(recent); err != nil {
		t.Errorf("expected recent directory to be kept: %v", err)
	}
}

func TestSweepOrphanedJails_IgnoresOtherPrefixes(t *testing.T) {
	base := t.TempDir()

	other := filepath.Join(base, "other-old123")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(other, past, past); err != nil {
		t.Fatal(err)
	}

	sweepOrphanedJails(base, "goboxd-", 10*time.Minute)

	if _, err := os.Stat(other); err != nil {
		t.Errorf("directory with non-matching prefix should not be removed: %v", err)
	}
}

func TestSweepOrphanedJails_NonexistentBase(t *testing.T) {
	// must not panic when baseDir does not exist
	sweepOrphanedJails(filepath.Join(t.TempDir(), "nonexistent"), "goboxd-", 10*time.Minute)
}

func TestSweepOrphanedJails_RemovesOnlyOldKeepsRecent(t *testing.T) {
	base := t.TempDir()

	old := filepath.Join(base, "goboxd-stale")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(base, "goboxd-fresh")
	if err := os.Mkdir(fresh, 0700); err != nil {
		t.Fatal(err)
	}

	sweepOrphanedJails(base, "goboxd-", 10*time.Minute)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale directory should have been removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh directory should remain: %v", err)
	}
}
