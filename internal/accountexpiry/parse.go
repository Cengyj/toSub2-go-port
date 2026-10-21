// Package accountexpiry parses user-entered account use deadlines. These dates
// are independent of OAuth token lifetimes and never authorize account actions.
package accountexpiry

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA browser timezones available in pure-Go deployments.
)

var shortTime = regexp.MustCompile(`^([01]?[0-9]|2[0-3])[.:]([0-5][0-9])$`)
var monthDayTime = regexp.MustCompile(`^([0-9]{1,2})-([0-9]{1,2})[ T]([0-9]{1,2}):([0-9]{2})(?::([0-9]{2}))?$`)

// Location accepts a browser IANA zone, falling back to the service's local
// timezone for older clients. Never include supplied text in validation errors.
func Location(zone string) (*time.Location, error) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return time.Local, nil
	}
	if len(zone) > 100 || strings.Contains(zone, "..") || strings.HasPrefix(zone, "/") || strings.ContainsAny(zone, "\\\x00") {
		return nil, errors.New("到期时间的时区无效，请刷新页面后重试")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("到期时间的时区无效，请刷新页面后重试")
	}
	return loc, nil
}

// ParseManual resolves short wall-clock times and month/day dates against one
// captured import date in the browser's timezone. Past times stay expired;
// neither the day nor the year is rolled forward.
func ParseManual(raw, zone string, now time.Time) (time.Time, error) {
	loc, err := Location(zone)
	if err != nil {
		return time.Time{}, err
	}
	value := strings.TrimSpace(raw)
	if len(value) > 80 {
		return time.Time{}, invalidTime()
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil && !parsed.IsZero() && parsed.UTC().Year() >= 1 && parsed.UTC().Year() <= 9999 {
		return parsed.UTC(), nil
	}
	if match := shortTime.FindStringSubmatch(value); match != nil {
		hour, _ := strconv.Atoi(match[1])
		minute, _ := strconv.Atoi(match[2])
		local := now.In(loc)
		value = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, time.UTC).Format("2006-01-02 15:04")
	} else if match := monthDayTime.FindStringSubmatch(value); match != nil {
		month, _ := strconv.Atoi(match[1])
		day, _ := strconv.Atoi(match[2])
		hour, _ := strconv.Atoi(match[3])
		// Format first, then use the same strict calendar and DST checks as
		// full dates below. time.Date here would normalize invalid dates.
		value = fmt.Sprintf("%04d-%02d-%02d %02d:%s", now.In(loc).Year(), month, day, hour, match[4])
		if match[5] != "" {
			value += ":" + match[5]
		}
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		parsed, err := time.ParseInLocation(layout, value, loc)
		if err != nil || parsed.IsZero() || parsed.UTC().Year() < 1 || parsed.UTC().Year() > 9999 || parsed.Format(layout) != value {
			continue
		}
		// A repeated DST wall time identifies two different instants. Require
		// an explicit RFC3339 offset instead of silently choosing one.
		_, offset := parsed.Zone()
		for _, delta := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
			_, adjacent := parsed.Add(delta).Zone()
			if adjacent != offset && parsed.Add(time.Duration(offset-adjacent)*time.Second).In(loc).Format(layout) == value {
				return time.Time{}, errors.New("到期时间处于夏令时重复时段，请填写带时区偏移的完整时间")
			}
		}
		return parsed.UTC(), nil
	}
	return time.Time{}, invalidTime()
}

func invalidTime() error {
	return errors.New("到期时间无效，请使用 16.30、16:30、7-18 8:00 或 2026-10-20 16:30；未写年份使用浏览器时区的导入当年，不顺延；也可填写带时区的完整时间")
}
