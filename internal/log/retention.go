package log

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RetentionPolicy string

const (
	RetentionThisWeek  RetentionPolicy = "this_week"
	RetentionThisMonth RetentionPolicy = "this_month"
	RetentionNMonths   RetentionPolicy = "n_months"
	RetentionForever   RetentionPolicy = "forever"
)

func applyRetention(logDir string, policy RetentionPolicy, months int) {
	switch policy {
	case RetentionForever:
		return
	case RetentionThisWeek:
		cleanupExceptCurrentWeek(logDir)
	case RetentionThisMonth:
		cleanupExceptCurrentMonth(logDir)
	case RetentionNMonths:
		cleanupBeforeMonths(logDir, months)
	}
}

func cleanupExceptCurrentWeek(logDir string) {
	now := time.Now()
	keep := lastWeekKey(now)
	for _, f := range listArchives(logDir, "week_") {
		if !strings.HasPrefix(f, "week_"+keep) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	cleanupAll(logDir, "month_")
	cleanupAll(logDir, "year_")
}

func cleanupExceptCurrentMonth(logDir string) {
	now := time.Now()
	keep := lastMonthKey(now)
	for _, f := range listArchives(logDir, "month_") {
		if !strings.HasPrefix(f, "month_"+keep) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	cleanupAll(logDir, "year_")
}

func cleanupBeforeMonths(logDir string, months int) {
	cutoff := time.Now().AddDate(0, -months, 0)
	for _, f := range listArchives(logDir, "month_") {
		// month_2006-01.tar.gz
		if len(f) < 15 {
			continue
		}
		dateStr := f[6:13]
		t, err := time.Parse("2006-01", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	for _, f := range listArchives(logDir, "year_") {
		if len(f) < 12 {
			continue
		}
		yearStr := f[5:9]
		y, err := time.Parse("2006", yearStr)
		if err != nil {
			continue
		}
		if y.Before(cutoff) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
}

func cleanupAll(logDir, prefix string) {
	for _, f := range listArchives(logDir, prefix) {
		if err := os.Remove(filepath.Join(logDir, f)); err != nil {
			log.Printf("[log retention] remove %s: %v", f, err)
		}
	}
}
