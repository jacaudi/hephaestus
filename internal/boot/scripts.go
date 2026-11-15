package boot

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupportedAction is returned by Render for an action whose script M1
// does not have.
var ErrUnsupportedAction = errors.New("boot: script for this action is not available in M1")

// ErrUnknownAction is returned by Render for an action that is not one of the
// Action constants, so a decision that was never set cannot render as an empty script.
var ErrUnknownAction = errors.New("boot: unknown action")

// bootQuery is the /boot query string. Request facts reach it only as iPXE
// variables, never as text.
const bootQuery = "uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch}" +
	"&serial=${serial:uristring}&product=${product:uristring}"

// reportTemplate posts the machine's facts to /discover; <fqdn> is replaced by the FQDN.
const reportTemplate = `params
param uuid ${uuid}
param mac ${net${idx}/mac}
param serial ${serial}
param manufacturer ${manufacturer}
param product ${product}
param cpuvendor ${cpuvendor}
param cpumodel ${cpumodel}
param memsize ${memsize}
param platform ${platform}
param arch ${buildarch}
set nic:int32 0
:hephaestus_nic
isset ${net${nic}/mac} || goto hephaestus_nic_done
param nic${nic}_mac ${net${nic}/mac}
param nic${nic}_chip ${net${nic}/chip}
inc nic
goto hephaestus_nic
:hephaestus_nic_done
imgfetch --name hephaestus-report https://<fqdn>/discover##params || echo hephaestus: discovery report failed
imgfree hephaestus-report ||
`

const ipxeHeader = "#!ipxe\n"

// StartScript chains the machine to /boot over TLS. It is also row 1's script.
func StartScript(fqdn string) string {
	return ipxeHeader + "chain https://" + fqdn + "/boot?" + bootQuery + "\n"
}

// ExitScript hands the machine back to its firmware.
func ExitScript() string {
	return ipxeHeader + "exit\n"
}

// HoldScript prints reason, waits 30 seconds and asks /boot again.
func HoldScript(fqdn, reason string) string {
	return ipxeHeader + "echo hephaestus: holding: " + reason + "\nsleep 30\n" +
		"chain --replace https://" + fqdn + "/boot?" + bootQuery + "\n"
}

// ReportScript runs the discovery report, then the script then.
func ReportScript(fqdn, then string) string {
	return ipxeHeader + reportBlock(fqdn) + strings.TrimPrefix(then, ipxeHeader)
}

// Render returns the script for d. Utility and install scripts belong to M2
// and M3; Render returns ErrUnsupportedAction for them, and ErrUnknownAction
// for an action it does not know.
func Render(d Decision, fqdn string) (string, error) {
	var script string
	switch d.Action {
	case ActionChainTLS:
		script = StartScript(fqdn)
	case ActionExit:
		script = ExitScript()
	case ActionHold:
		script = HoldScript(fqdn, d.Reason)
	case ActionUtility, ActionInstall:
		return "", ErrUnsupportedAction
	default:
		return "", fmt.Errorf("%w %q", ErrUnknownAction, d.Action)
	}
	if d.Report {
		script = ReportScript(fqdn, script)
	}
	return script, nil
}

func reportBlock(fqdn string) string {
	return strings.ReplaceAll(reportTemplate, "<fqdn>", fqdn)
}
