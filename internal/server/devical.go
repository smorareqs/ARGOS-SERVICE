package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Calendarios iCal de demostracion, solo en local (ARGOS_ENVIRONMENT=local):
// GET /dev/ical/{archivo}. La semilla registra estas URLs en las propiedades
// de Garrent y Costa Limpia para probar "Sincronizar iCal" sin salir a
// internet; las fechas se calculan desde hoy para que la demo no envejezca.

// DemoEvent es una estancia de un calendario de demostracion (dias desde hoy).
type DemoEvent struct {
	UID      string
	From, To int
	Summary  string
}

// DemoFeeds son los calendarios por archivo. Los UID que la semilla ya creo
// como reservas no cambian nada al sincronizar; los demas se importan.
var DemoFeeds = map[string][]DemoEvent{
	"garrent-an-seb-booking.ics": {
		{"bk-anseb-88213", 3, 7, "CLOSED - Not available"},
		{"bk-anseb-88402", 15, 18, "CLOSED - Not available"},
	},
	"garrent-at-vis-airbnb.ics": {
		{"ab-atvis-HMX4Q2", -1, 4, "Reserved"},
		{"ab-atvis-HMR7K1", 6, 10, "Reserved"},
		{"ab-atvis-block", 20, 24, "Airbnb (Not available)"},
	},
	"garrent-tb-504-airbnb.ics": {
		{"ab-tb504-HMB2N8", 5, 9, "Reserved"},
	},
	"garrent-cy-08c-airbnb.ics": {
		{"ab-cy08c-HMT9P4", 0, 5, "Reserved"},
		{"ab-cy08c-HMW3J6", 9, 12, "Reserved"},
	},
	"costa_limpia-mb-5a-booking.ics": {
		{"bk-mb5a-77120", 2, 6, "CLOSED - Not available"},
		{"bk-mb5a-77305", 11, 14, "CLOSED - Not available"},
	},
}

func mountDevICal(router chi.Router) {
	gt, err := time.LoadLocation("America/Guatemala")
	if err != nil {
		gt = time.UTC
	}
	router.Get("/dev/ical/{file}", func(w http.ResponseWriter, r *http.Request) {
		events, ok := DemoFeeds[chi.URLParam(r, "file")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		today := time.Now().In(gt)
		day := func(n int) string { return today.AddDate(0, 0, n).Format("20060102") }
		var b strings.Builder
		b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//ARGOS//Demo iCal//ES\r\n")
		for _, e := range events {
			fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:%s\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\nSUMMARY:%s\r\nEND:VEVENT\r\n",
				e.UID, day(e.From), day(e.To), e.Summary)
		}
		b.WriteString("END:VCALENDAR\r\n")
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}
