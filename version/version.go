package version

import "fmt"

var (
	defaultVersionString = "0.0.0-git"
	versionString        = ""
	commit               = ""
	date                 = ""
)

// Info is a struct that contains informations about the application
type Info struct {
	Application   string `json:"Application"`
	VersionString string `json:"VersionString"`
	Commit        string `json:"Commit"`
	Date          string `json:"Date"`
}

// NewInfo returns a pointer to an updated Info struct
func NewInfo(application string) *Info {
	return &Info{
		Application:   application,
		VersionString: versionString,
		Commit:        commit,
		Date:          date,
	}
}

func (i *Info) String() string {
	return fmt.Sprintf("%[1]s Version: %[2]s Commit: %[3]s Date: %[4]s", i.Application, i.VersionString, i.Commit, i.Date)
}

//nolint:gochecknoinits
func init() {
	if versionString == "" {
		versionString = defaultVersionString
	}
}
