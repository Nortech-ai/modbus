package modbus

import "fmt"

type ReportSlaveId struct {
	SlaveId          byte
	RunIndicator     byte
	OtherInformation []byte
}

func ParseReportSlaveId(raw []byte) (ReportSlaveId, error) {
	if len(raw) < 4 {
		return ReportSlaveId{}, fmt.Errorf("modbus: invalid report slave id response length: %d", len(raw))
	}

	reportSlaveId := ReportSlaveId{}
	reportSlaveId.SlaveId = raw[0]
	reportSlaveId.RunIndicator = raw[1]
	reportSlaveId.OtherInformation = raw[2:]
	return reportSlaveId, nil
}
