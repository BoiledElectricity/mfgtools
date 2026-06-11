package uuurun

import (
	"reflect"
	"testing"
)

func TestParseVerboseOutput(t *testing.T) {
	lines := []string{
		"uuu (Universal Update Utility) for nxp imx chips -- libuuu_1.5.233-0-g79ce7d2",
		"Wait for Known USB Device Appear... \\",
		"New USB Device Attached at 1:16",
		"1:16>Start Cmd:SDPS: boot -f pmm.wic -scanterm -scanlimited 0x800000",
		"\x1b[32m1:16>Okay (0.53s)\x1b[0m",
		"1:16>Start Cmd:FB: flash -raw2sparse all pmm.wic",
		"\x1b[33m42%\x1b[0m",
		"100%",
		"1:16>Okay (95.2s)",
		"1:16>Start Cmd:FB: done",
		"1:16>Fail Bulk(W):LIBUSB_ERROR_IO(2.1s)",
	}
	var got []Event
	p := &parser{fn: func(e Event) { got = append(got, e) }}
	for _, l := range lines {
		p.line(l)
	}

	want := []Event{
		{Type: "line", Text: "uuu (Universal Update Utility) for nxp imx chips -- libuuu_1.5.233-0-g79ce7d2"},
		{Type: "wait", Text: "Wait for Known USB Device Appear..."},
		{Type: "attach", Dev: "1:16", Text: "New USB Device Attached at 1:16"},
		{Type: "cmdstart", Dev: "1:16", Text: "SDPS: boot -f pmm.wic -scanterm -scanlimited 0x800000"},
		{Type: "cmdok", Dev: "1:16", Text: "1:16>Okay (0.53s)"},
		{Type: "cmdstart", Dev: "1:16", Text: "FB: flash -raw2sparse all pmm.wic"},
		{Type: "progress", Dev: "1:16", Percent: 42},
		{Type: "progress", Dev: "1:16", Percent: 100},
		{Type: "cmdok", Dev: "1:16", Text: "1:16>Okay (95.2s)"},
		{Type: "cmdstart", Dev: "1:16", Text: "FB: done"},
		{Type: "cmdfail", Dev: "1:16", Text: "Bulk(W):LIBUSB_ERROR_IO(2.1s)"},
	}
	if !reflect.DeepEqual(got, want) {
		for i := range got {
			if i >= len(want) || got[i] != want[i] {
				t.Errorf("event %d:\n got %+v", i, got[i])
				if i < len(want) {
					t.Errorf("want %+v", want[i])
				}
			}
		}
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
}

func TestParseLsusb(t *testing.T) {
	out := "uuu (Universal Update Utility) for nxp imx chips\n\n" +
		"Connected Known USB Devices\n" +
		"\tPath\t Chip\t Pro\t Vid\t Pid\t BcdVersion\t Serial No\n" +
		"\t==================================================================\n" +
		"\t1:16\t MX8MN\t SDPS:\t 0x1FC9\t0x0132\t 0x0001\t XXXX\n"
	devs := parseLsusb(out)
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	d := devs[0]
	if d.Path != "1:16" || d.Chip != "MX8MN" || d.Pro != "SDPS" || d.Vid != "0x1FC9" || d.Pid != "0x0132" {
		t.Fatalf("bad parse: %+v", d)
	}
}
