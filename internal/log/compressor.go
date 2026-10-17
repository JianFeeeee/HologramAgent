package log

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var rawLogRe = regexp.MustCompile(`^homed_(\d{4}-\d{2}-\d{2})_\d{2}-\d{2}-\d{2}\.log$`)
var weekRe = regexp.MustCompile(`^week_(\d{4})-W(\d{2})\.tar\.gz$`)
var monthRe = regexp.MustCompile(`^month_(\d{4}-\d{2})\.tar\.gz$`)

type fileGroup struct {
	name  string
	files []string
}

func compressWeekly(logDir string) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return err
	}

	groups := make(map[string][]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := rawLogRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		t, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		y, w := t.ISOWeek()
		key := fmt.Sprintf("%04d-W%02d", y, w)
		groups[key] = append(groups[key], e.Name())
	}

	for key, files := range groups {
		dst := filepath.Join(logDir, fmt.Sprintf("week_%s.tar.gz", key))
		if fileExists(dst) {
			continue
		}
		sort.Strings(files)
		if err := tarGzFiles(logDir, dst, files); err != nil {
			return fmt.Errorf("compress week %s: %w", key, err)
		}
		for _, f := range files {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	return nil
}

func compressMonthly(logDir string) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return err
	}

	groups := make(map[string][]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := weekRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		key := fmt.Sprintf("%s-%s", m[1], m[2][:2])
		monthKey := key[:7]
		groups[monthKey] = append(groups[monthKey], e.Name())
	}

	for monthKey, files := range groups {
		dst := filepath.Join(logDir, fmt.Sprintf("month_%s.tar.gz", monthKey))
		if fileExists(dst) {
			continue
		}
		sort.Strings(files)
		if err := tarGzFiles(logDir, dst, files); err != nil {
			return fmt.Errorf("compress month %s: %w", monthKey, err)
		}
		for _, f := range files {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	return nil
}

func compressYearly(logDir string) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return err
	}

	groups := make(map[string][]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := monthRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		yearKey := m[1][:4]
		groups[yearKey] = append(groups[yearKey], e.Name())
	}

	for yearKey, files := range groups {
		dst := filepath.Join(logDir, fmt.Sprintf("year_%s.tar.gz", yearKey))
		if fileExists(dst) {
			continue
		}
		sort.Strings(files)
		if err := tarGzFiles(logDir, dst, files); err != nil {
			return fmt.Errorf("compress year %s: %w", yearKey, err)
		}
		for _, f := range files {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	return nil
}

func tarGzFiles(baseDir, dst string, filenames []string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, fn := range filenames {
		path := filepath.Join(baseDir, fn)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			continue
		}
		header.Name = fn
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, r); err != nil {
			r.Close()
			return err
		}
		r.Close()
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lastWeekKey(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

func lastMonthKey(t time.Time) string {
	return t.Format("2006-01")
}

func lastYearKey(t time.Time) string {
	return t.Format("2006")
}

func parseWeekKey(key string) (time.Time, error) {
	var y, w int
	if _, err := fmt.Sscanf(key, "%04d-W%02d", &y, &w); err != nil {
		return time.Time{}, err
	}
	return firstDayOfISOWeek(y, w), nil
}

func firstDayOfISOWeek(year, week int) time.Time {
	jan1 := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	_, jan1Week := jan1.ISOWeek()
	daysOffset := (week - jan1Week) * 7
	t := jan1.AddDate(0, 0, daysOffset)
	for t.Weekday() != time.Monday {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

func listArchives(logDir, prefix string) []string {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}
	var result []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			result = append(result, e.Name())
		}
	}
	return result
}
