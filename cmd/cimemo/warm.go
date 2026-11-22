// Warm mode: replay git history through cimemo to pre-seed the cache.
//
//	cimemo warm --last 10 make          # replay last 10 commits
//	cimemo warm --since v1.2.0 make     # replay from a tag
//	cimemo warm --shas abc,def make     # specific commits
//
// Each commit is checked out, the build command runs through cimemo,
// and receipts accumulate (locally or in the remote blob store). The
// report shows the warming curve: how many commits until the cache is
// mostly hits, and the time saved vs cold builds.
//
// Use cases:
//   - new CI runner: warm before PRs arrive
//   - cache recovery: re-seed after a clear or upgrade
//   - demonstration: prove the system works on any project's history
//   - paper evidence: controlled experiments with known timings
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WarmStats records per-commit timing for the warming curve.
type WarmStats struct {
	Commit   string
	Command  string
	Cold     bool // true = built from scratch, false = receipt hit
	WallMs   int64
	Outputs  int
	Strategy string
}

func cmdWarm(args []string) {
	// parse flags
	var lastN int
	var sinceRef string
	var shas []string
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--last" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &lastN)
			i++
		} else if a == "--since" && i+1 < len(args) {
			sinceRef = args[i+1]
			i++
		} else if a == "--shas" && i+1 < len(args) {
			shas = strings.Split(args[i+1], ",")
			i++
		} else {
			cmdArgs = append(cmdArgs, a)
		}
	}

	if len(cmdArgs) == 0 {
		fatal("warm: specify a build command (e.g., cimemo warm --last 5 make)")
	}

	wd, _ := os.Getwd()
	command := strings.Join(cmdArgs, " ")

	// resolve commits to replay
	var commits []string
	switch {
	case lastN > 0:
		out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"log", "--format=%H", fmt.Sprintf("-%d", lastN)).Output()
		if err != nil {
			fatal("git log: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				commits = append(commits, line)
			}
		}
		// reverse: oldest first (so each build benefits from the previous)
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
	case sinceRef != "":
		out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"log", "--format=%H", fmt.Sprintf("%s..HEAD", sinceRef)).Output()
		if err != nil {
			fatal("git log %s..HEAD: %v", sinceRef, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				commits = append(commits, line)
			}
		}
		// reverse to oldest first
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
	case len(shas) > 0:
		commits = shas
	default:
		fatal("warm: specify --last N, --since <ref>, or --shas a,b,c")
	}

	if len(commits) == 0 {
		fatal("warm: no commits to replay")
	}

	fmt.Printf("[cimemo] warm: replaying %d commit(s) through: %s\n", len(commits), command)

	// save the current HEAD to restore later
	origHead, _ := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
		"rev-parse", "HEAD").Output()
	origHeadStr := strings.TrimSpace(string(origHead))
	defer func() {
		if origHeadStr != "" {
			exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
				"checkout", "-q", origHeadStr).Run()
		}
	}()

	var stats []WarmStats
	var totalCold, totalWarm int64
	var coldCount, warmCount int

	for idx, sha := range commits {
		short := sha[:12]
		fmt.Printf("\n[%d/%d] %s: ", idx+1, len(commits), short)

		// checkout
		if err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"checkout", "-q", sha).Run(); err != nil {
			fmt.Printf("checkout failed: %v\n", err)
			continue
		}

		// run through cimemo
		start := time.Now()
		cimemoSelf, _ := os.Executable()
		cmd := exec.Command(cimemoSelf, cmdArgs...)
		cmd.Dir = wd
		cmd.Env = os.Environ()

		// CombinedOutput sets Stdout/Stderr internally — don't set them
		out, err := cmd.CombinedOutput()
		wall := time.Since(start).Milliseconds()

		stat := WarmStats{
			Commit:  short,
			Command: command,
			WallMs:  wall,
		}

		output := string(out)
		if strings.Contains(output, "CACHE HIT") {
			stat.Cold = false
			warmCount++
			totalWarm += wall
			fmt.Printf("HIT (%dms)\n", wall)
		} else {
			stat.Cold = true
			coldCount++
			totalCold += wall
			fmt.Printf("MISS → built (%dms)\n", wall)
			if err != nil {
				fmt.Printf("  build error: %v\n", err)
			}
		}

		// extract strategy and output count from cimemo output
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "strategy=") {
				parts := strings.Split(line, "strategy=")
				if len(parts) > 1 {
					stat.Strategy = strings.TrimSpace(strings.Split(parts[1], " ")[0])
				}
			}
			if strings.Contains(line, "outputs") {
				var n int
				fmt.Sscanf(line, "  %d outputs", &n)
				stat.Outputs = n
			}
		}

		stats = append(stats, stat)
	}

	// summary
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("[cimemo] warm summary: %d commits replayed\n", len(stats))
	fmt.Printf("[cimemo]   cold builds: %d (%.1f%%) — total %dms\n",
		coldCount, float64(coldCount)/float64(len(stats))*100, totalCold)
	fmt.Printf("[cimemo]   warm hits:   %d (%.1f%%) — total %dms\n",
		warmCount, float64(warmCount)/float64(len(stats))*100, totalWarm)

	if coldCount > 0 && warmCount > 0 {
		avgCold := totalCold / int64(coldCount)
		avgWarm := totalWarm / int64(warmCount)
		fmt.Printf("[cimemo]   avg cold: %dms, avg warm: %dms — %.1fx speedup on hits\n",
			avgCold, avgWarm, float64(avgCold)/float64(avgWarm))
	}

	// warming curve (per-commit: cumulative hit rate)
	fmt.Printf("[cimemo] warming curve:\n")
	cumCold, cumWarm := 0, 0
	for i, s := range stats {
		if s.Cold {
			cumCold++
		} else {
			cumWarm++
		}
		hitRate := float64(cumWarm) / float64(i+1) * 100
		status := "COLD"
		if !s.Cold {
			status = "warm"
		}
		fmt.Printf("[cimemo]   %s  %s  %5dms  hit-rate: %5.1f%%\n",
			s.Commit[:8], status, s.WallMs, hitRate)
	}
}
