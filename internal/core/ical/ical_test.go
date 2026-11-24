package ical

import (
	"errors"
	"net"
	"testing"
	"time"
)

const airbnb = "BEGIN:VCALENDAR\r\nPRODID;X-RICAL-TZSOURCE=TZINFO:-//Airbnb Inc//Hosting Calendar 1.0//EN\r\nVERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\nDTEND;VALUE=DATE:20261208\r\nDTSTART;VALUE=DATE:20261205\r\nUID:1418fb94e984-0f7b@airbnb.com\r\n" +
	"DESCRIPTION:Reservation URL: https://www.airbnb.com/hosting/reservations/details/HMABC\\nPhone\r\n" +
	" Number (Last 4 Digits): 1234\r\nSUMMARY:Reserved\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nDTEND;VALUE=DATE:20261220\r\nDTSTART;VALUE=DATE:20261215\r\nUID:7f5e@airbnb.com\r\n" +
	"SUMMARY:Airbnb (Not available)\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestParseAirbnb(t *testing.T) {
	evs, err := Parse(airbnb, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("eventos = %d, quería 2", len(evs))
	}
	e := evs[0]
	if e.UID != "1418fb94e984-0f7b@airbnb.com" || e.Start != "2026-12-05" || e.End != "2026-12-08" || e.Summary != "Reserved" {
		t.Fatalf("evento inesperado: %+v", e)
	}
}

func TestParseVariants(t *testing.T) {
	gt, _ := time.LoadLocation("America/Guatemala")
	body := "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:a\nDTSTART:20261205T050000Z\nDTEND:20261207T050000Z\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:b\nDTSTART;TZID=America/Guatemala:20261210T150000\nDURATION:P3D\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:c\nDTSTART;VALUE=DATE:20261220\nSTATUS:CANCELLED\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nDTSTART;VALUE=DATE:20261220\nEND:VEVENT\nEND:VCALENDAR\n"
	evs, err := Parse(body, gt)
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{UID: "a", Start: "2026-12-04", End: "2026-12-06"}, // 05:00Z es 23:00 del dia anterior en Guatemala
		{UID: "b", Start: "2026-12-10", End: "2026-12-13"},
		{UID: "c", Start: "2026-12-20", End: "2026-12-21", Cancelled: true},
	}
	if len(evs) != len(want) {
		t.Fatalf("eventos = %+v", evs)
	}
	for i, w := range want {
		if evs[i] != w {
			t.Errorf("evento %d = %+v, quería %+v", i, evs[i], w)
		}
	}
}

func TestParseRejectsHTML(t *testing.T) {
	if _, err := Parse("<html>login</html>", time.UTC); !errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v, quería ErrFormat", err)
	}
}

func TestValidURL(t *testing.T) {
	f := NewFetcher(false)
	for raw, ok := range map[string]bool{
		"https://www.airbnb.com/calendar/ical/1.ics?s=abc": true,
		"webcal://calendar.example.com/feed.ics":           true,
		"http://example.com/feed.ics":                      false,
		"ftp://example.com/feed.ics":                       false,
		"https://user:pass@example.com/feed.ics":           false,
		"no es una url":                                    false,
	} {
		_, err := f.ValidURL(raw)
		if (err == nil) != ok {
			t.Errorf("ValidURL(%q) err = %v", raw, err)
		}
	}
	if _, err := NewFetcher(true).ValidURL("http://localhost:8080/feed.ics"); err != nil {
		t.Errorf("en local http debe valer: %v", err)
	}
}

func TestPrivateIP(t *testing.T) {
	for ip, private := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "192.168.0.10": true, "172.20.0.5": true, "169.254.169.254": true,
		"100.64.0.1": true, "::1": true, "fd00::1": true, "8.8.8.8": false, "34.120.5.9": false,
	} {
		if got := privateIP(net.ParseIP(ip)); got != private {
			t.Errorf("privateIP(%s) = %v", ip, got)
		}
	}
}
