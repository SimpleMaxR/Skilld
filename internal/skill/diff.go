package skill

import (
	"fmt"
	"sort"
	"strings"
)

const maxDiffLinesPerFile = 1200

// DiffLine represents a single line inside a unified diff hunk.
type DiffLine struct {
	Type      string `json:"type"`
	LeftLine  *int   `json:"leftLine"`
	RightLine *int   `json:"rightLine"`
	Text      string `json:"text"`
}

// DiffHunk represents a contiguous block of changes with context lines.
type DiffHunk struct {
	Header string     `json:"header"`
	Lines  []DiffLine `json:"lines"`
}

// FileDiff represents the per-file diff state between left and right snapshots.
type FileDiff struct {
	Path        string     `json:"path"`
	Status      string     `json:"status"`
	Binary      bool       `json:"binary"`
	ModeChanged bool       `json:"modeChanged"`
	LeftMode    *string    `json:"leftMode"`
	RightMode   *string    `json:"rightMode"`
	Hunks       []DiffHunk `json:"hunks"`
}

// DiffResult summarizes differences across two skill file maps.
type DiffResult struct {
	HasChanges   bool       `json:"hasChanges"`
	ChangedCount int        `json:"changedCount"`
	Files        []FileDiff `json:"files"`
}

// DiffFileMaps compares left (base/local) against right (target/vault) and produces line-level hunks.
func DiffFileMaps(leftInput, rightInput map[string]FileEntry) (DiffResult, error) {
	var left map[string]FileEntry
	var right map[string]FileEntry
	var err error

	if leftInput != nil {
		left, err = NormalizeFileMap(leftInput)
		if err != nil {
			return DiffResult{}, err
		}
	} else {
		left = map[string]FileEntry{}
	}
	if rightInput != nil {
		right, err = NormalizeFileMap(rightInput)
		if err != nil {
			return DiffResult{}, err
		}
	} else {
		right = map[string]FileEntry{}
	}

	pathSet := make(map[string]struct{}, len(left)+len(right))
	for p := range left {
		pathSet[p] = struct{}{}
	}
	for p := range right {
		pathSet[p] = struct{}{}
	}
	allPaths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	files := make([]FileDiff, 0, len(allPaths))
	changedCount := 0

	for _, relPath := range allPaths {
		l, hasL := left[relPath]
		r, hasR := right[relPath]

		if !hasL && hasR {
			changedCount++
			files = append(files, buildSingleSideDiff(relPath, "added", r, "+"))
			continue
		}
		if hasL && !hasR {
			changedCount++
			files = append(files, buildSingleSideDiff(relPath, "removed", l, "-"))
			continue
		}
		if l.Encoding == r.Encoding && l.Mode == r.Mode && l.Content == r.Content {
			lm, rm := l.Mode, r.Mode
			files = append(files, FileDiff{
				Path:        relPath,
				Status:      "unchanged",
				Binary:      l.Encoding == "base64",
				ModeChanged: false,
				LeftMode:    &lm,
				RightMode:   &rm,
				Hunks:       make([]DiffHunk, 0),
			})
			continue
		}

		changedCount++
		isBinary := l.Encoding == "base64" || r.Encoding == "base64"
		modeChanged := l.Mode != r.Mode
		lm, rm := l.Mode, r.Mode

		if isBinary {
			files = append(files, FileDiff{
				Path:        relPath,
				Status:      "modified",
				Binary:      true,
				ModeChanged: modeChanged,
				LeftMode:    &lm,
				RightMode:   &rm,
				Hunks:       make([]DiffHunk, 0),
			})
			continue
		}

		hunks := make([]DiffHunk, 0)
		if l.Content != r.Content {
			hunks = computeUnifiedHunks(splitDiffLines(l.Content), splitDiffLines(r.Content), 3)
		}
		files = append(files, FileDiff{
			Path:        relPath,
			Status:      "modified",
			Binary:      false,
			ModeChanged: modeChanged,
			LeftMode:    &lm,
			RightMode:   &rm,
			Hunks:       hunks,
		})
	}

	return DiffResult{
		HasChanges:   changedCount > 0,
		ChangedCount: changedCount,
		Files:        files,
	}, nil
}

func buildSingleSideDiff(relPath, status string, entry FileEntry, prefix string) FileDiff {
	var leftMode, rightMode *string
	modeCopy := entry.Mode
	if status == "removed" {
		leftMode = &modeCopy
	} else {
		rightMode = &modeCopy
	}

	if entry.Encoding == "base64" {
		return FileDiff{
			Path:      relPath,
			Status:    status,
			Binary:    true,
			LeftMode:  leftMode,
			RightMode: rightMode,
			Hunks:     make([]DiffHunk, 0),
		}
	}

	lines := splitDiffLines(entry.Content)
	cappedLen := len(lines)
	if cappedLen > maxDiffLinesPerFile {
		cappedLen = maxDiffLinesPerFile
	}
	diffLines := make([]DiffLine, 0, cappedLen+1)
	for idx := 0; idx < cappedLen; idx++ {
		ln := idx + 1
		if prefix == "+" {
			diffLines = append(diffLines, DiffLine{Type: "add", RightLine: &ln, Text: lines[idx]})
		} else {
			diffLines = append(diffLines, DiffLine{Type: "del", LeftLine: &ln, Text: lines[idx]})
		}
	}
	if len(lines) > maxDiffLinesPerFile {
		diffLines = append(diffLines, DiffLine{
			Type: "meta",
			Text: fmt.Sprintf("... truncated (%d more lines)", len(lines)-maxDiffLinesPerFile),
		})
	}

	header := fmt.Sprintf("@@ -0,0 +1,%d @@", cappedLen)
	if status != "added" {
		header = fmt.Sprintf("@@ -1,%d +0,0 @@", cappedLen)
	}

	return FileDiff{
		Path:      relPath,
		Status:    status,
		Binary:    false,
		LeftMode:  leftMode,
		RightMode: rightMode,
		Hunks: []DiffHunk{
			{Header: header, Lines: diffLines},
		},
	}
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func computeUnifiedHunks(leftLines, rightLines []string, context int) []DiffHunk {
	if len(leftLines)*len(rightLines) > 400_000 {
		lCap := min(len(leftLines), 200)
		rCap := min(len(rightLines), 200)
		lines := make([]DiffLine, 0, lCap+rCap)
		for i := 0; i < lCap; i++ {
			ln := i + 1
			lines = append(lines, DiffLine{Type: "del", LeftLine: &ln, Text: leftLines[i]})
		}
		for i := 0; i < rCap; i++ {
			ln := i + 1
			lines = append(lines, DiffLine{Type: "add", RightLine: &ln, Text: rightLines[i]})
		}
		return []DiffHunk{
			{
				Header: fmt.Sprintf("@@ -1,%d +1,%d @@", len(leftLines), len(rightLines)),
				Lines:  lines,
			},
		}
	}

	ops := computeLineDiffOps(leftLines, rightLines)
	changeIndices := make([]int, 0)
	for i, op := range ops {
		if op.Type != "ctx" {
			changeIndices = append(changeIndices, i)
		}
	}
	if len(changeIndices) == 0 {
		return make([]DiffHunk, 0)
	}

	hunks := make([]DiffHunk, 0)
	groupStart := max(0, changeIndices[0]-context)
	groupEnd := min(len(ops)-1, changeIndices[0]+context)

	for _, idx := range changeIndices[1:] {
		start := max(0, idx-context)
		end := min(len(ops)-1, idx+context)
		if start <= groupEnd+1 {
			groupEnd = end
		} else {
			hunks = append(hunks, buildHunk(ops[groupStart:groupEnd+1]))
			groupStart = start
			groupEnd = end
		}
	}
	hunks = append(hunks, buildHunk(ops[groupStart:groupEnd+1]))
	return hunks
}

func buildHunk(slice []DiffLine) DiffHunk {
	var leftStart, rightStart *int
	leftCount := 0
	rightCount := 0
	for _, op := range slice {
		if op.LeftLine != nil {
			if leftStart == nil {
				v := *op.LeftLine
				leftStart = &v
			}
			leftCount++
		}
		if op.RightLine != nil {
			if rightStart == nil {
				v := *op.RightLine
				rightStart = &v
			}
			rightCount++
		}
	}
	ls, rs := 0, 0
	if leftStart != nil {
		ls = *leftStart
	}
	if rightStart != nil {
		rs = *rightStart
	}
	lines := make([]DiffLine, len(slice))
	copy(lines, slice)
	return DiffHunk{
		Header: fmt.Sprintf("@@ -%d,%d +%d,%d @@", ls, leftCount, rs, rightCount),
		Lines:  lines,
	}
}

func computeLineDiffOps(a, b []string) []DiffLine {
	n := len(a)
	m := len(b)
	width := m + 1
	dp := make([]int32, (n+1)*(m+1))

	for i := n - 1; i >= 0; i-- {
		row := i * width
		nextRow := (i + 1) * width
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[row+j] = dp[nextRow+j+1] + 1
			} else if dp[nextRow+j] >= dp[row+j+1] {
				dp[row+j] = dp[nextRow+j]
			} else {
				dp[row+j] = dp[row+j+1]
			}
		}
	}

	ops := make([]DiffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			li, rj := i+1, j+1
			ops = append(ops, DiffLine{Type: "ctx", LeftLine: &li, RightLine: &rj, Text: a[i]})
			i++
			j++
		} else if dp[(i+1)*width+j] >= dp[i*width+j+1] {
			li := i + 1
			ops = append(ops, DiffLine{Type: "del", LeftLine: &li, Text: a[i]})
			i++
		} else {
			rj := j + 1
			ops = append(ops, DiffLine{Type: "add", RightLine: &rj, Text: b[j]})
			j++
		}
	}
	for i < n {
		li := i + 1
		ops = append(ops, DiffLine{Type: "del", LeftLine: &li, Text: a[i]})
		i++
	}
	for j < m {
		rj := j + 1
		ops = append(ops, DiffLine{Type: "add", RightLine: &rj, Text: b[j]})
		j++
	}
	return ops
}
