package buildinfo

import "fmt"

var Version = "dev"
var Commit = "unknown"
var BuildDate = "unknown"

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

func Current() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
	}
}

func (info Info) String() string {
	if info.Version == "dev" || info.Commit == "unknown" || info.BuildDate == "unknown" {
		return info.Version
	}
	return fmt.Sprintf("%s (commit %s, built %s)", info.Version, info.Commit, info.BuildDate)
}
