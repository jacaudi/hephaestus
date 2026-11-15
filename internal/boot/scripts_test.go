package boot

import (
	"errors"
	"strings"
	"testing"
)

const testFQDN = "hephaestus.example.test"

func TestStartScript(t *testing.T) {
	want := "#!ipxe\nchain https://hephaestus.example.test/boot?uuid=${uuid}&mac=${net${idx}/mac:hexhyp}" +
		"&arch=${buildarch}&serial=${serial:uristring}&product=${product:uristring}\n"
	if got := StartScript(testFQDN); got != want {
		t.Errorf("StartScript() = %q, want %q", got, want)
	}
}

func TestHoldScript(t *testing.T) {
	want := "#!ipxe\necho hephaestus: holding: awaiting approval\nsleep 30\n" +
		"chain --replace https://hephaestus.example.test/boot?" + bootQuery + "\n"
	if got := HoldScript(testFQDN, "awaiting approval"); got != want {
		t.Errorf("HoldScript() = %q, want %q", got, want)
	}
}

func TestExitScript(t *testing.T) {
	if got, want := ExitScript(), "#!ipxe\nexit\n"; got != want {
		t.Errorf("ExitScript() = %q, want %q", got, want)
	}
}

func TestReportScript(t *testing.T) {
	got := ReportScript(testFQDN, HoldScript(testFQDN, awaitingReason))

	if !strings.HasPrefix(got, "#!ipxe\nparams\nparam uuid ${uuid}\n") {
		t.Errorf("ReportScript() prefix wrong: %q", got)
	}
	if !strings.Contains(got, "imgfetch --name hephaestus-report https://hephaestus.example.test/discover##params") {
		t.Errorf("ReportScript() has no imgfetch of /discover: %q", got)
	}
	wantSuffix := "echo hephaestus: holding: awaiting approval\nsleep 30\n" +
		"chain --replace https://hephaestus.example.test/boot?" + bootQuery + "\n"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("ReportScript() does not end with the hold script body: %q", got)
	}
	if n := strings.Count(got, "#!ipxe"); n != 1 {
		t.Errorf("ReportScript() has %d #!ipxe lines, want 1", n)
	}
}

func TestReportScript_ExitExact(t *testing.T) {
	want := "#!ipxe\n" +
		"params\n" +
		"param uuid ${uuid}\n" +
		"param mac ${net${idx}/mac}\n" +
		"param serial ${serial}\n" +
		"param manufacturer ${manufacturer}\n" +
		"param product ${product}\n" +
		"param cpuvendor ${cpuvendor}\n" +
		"param cpumodel ${cpumodel}\n" +
		"param memsize ${memsize}\n" +
		"param platform ${platform}\n" +
		"param arch ${buildarch}\n" +
		"set nic:int32 0\n" +
		":hephaestus_nic\n" +
		"isset ${net${nic}/mac} || goto hephaestus_nic_done\n" +
		"param nic${nic}_mac ${net${nic}/mac}\n" +
		"param nic${nic}_chip ${net${nic}/chip}\n" +
		"inc nic\n" +
		"goto hephaestus_nic\n" +
		":hephaestus_nic_done\n" +
		"imgfetch --name hephaestus-report https://hephaestus.example.test/discover##params" +
		" || echo hephaestus: discovery report failed\n" +
		"imgfree hephaestus-report ||\n" +
		"exit\n"
	if got := ReportScript(testFQDN, ExitScript()); got != want {
		t.Errorf("ReportScript(ExitScript()) = %q, want %q", got, want)
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		name string
		d    Decision
		want string
	}{
		{"chain_tls", Decision{Row: 1, Action: ActionChainTLS}, StartScript(testFQDN)},
		{"exit", Decision{Row: 6, Action: ActionExit}, ExitScript()},
		{"hold", Decision{Row: 5, Action: ActionHold, Reason: "machine is cordoned"},
			HoldScript(testFQDN, "machine is cordoned")},
		{"report exit", Decision{Row: 7, Action: ActionExit, Report: true},
			ReportScript(testFQDN, ExitScript())},
		{"report hold", Decision{Row: 7, Action: ActionHold, Report: true, Reason: awaitingReason},
			ReportScript(testFQDN, HoldScript(testFQDN, awaitingReason))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.d, testFQDN)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Render() = %q, want %q", got, tc.want)
			}
		})
	}

	for _, d := range []Decision{
		{Row: 3, Action: ActionUtility, Utility: "memtest"},
		{Row: 10, Action: ActionInstall, NeedsTicket: true},
	} {
		t.Run("unsupported "+string(d.Action), func(t *testing.T) {
			if _, err := Render(d, testFQDN); !errors.Is(err, ErrUnsupportedAction) {
				t.Errorf("Render() error = %v, want ErrUnsupportedAction", err)
			}
		})
	}
}
