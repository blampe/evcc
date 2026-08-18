package api

// BatteryMode is the home battery operation mode. Valid values are normal, hold, charge, holdcharge and discharge
type BatteryMode int

//go:generate go tool enumer -type BatteryMode -trimprefix Battery -transform=lower
const (
	BatteryUnknown BatteryMode = iota
	BatteryNormal
	BatteryHold
	BatteryCharge
	BatteryHoldCharge
	// BatteryDischarge actively discharges the home battery to the grid (export
	// arbitrage). Not all battery controllers support it.
	BatteryDischarge
)
