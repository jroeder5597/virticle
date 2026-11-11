package api

import (
	"fmt"
	"regexp"
	"strings"

	"virticle/internal/cloudinit"
)

// unattendLabel is the volume label of the answer-file ISO.
const unattendLabel = "UNATTEND"

type unattendConfig struct {
	ComputerName string
	User         string
	Password     string
}

var (
	winComputerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,14}$`)
	// Windows forbids these in account names.
	winUserBad = `"/\[]:;|=,+*?<>@`
)

func (c unattendConfig) validate() error {
	if !winComputerRE.MatchString(c.ComputerName) || strings.Trim(c.ComputerName, "0123456789") == "" {
		return fmt.Errorf("invalid Windows computer name %q: at most 15 letters, digits and dashes, not only digits", c.ComputerName)
	}
	if c.User == "" || len(c.User) > 20 || strings.ContainsAny(c.User, winUserBad) || strings.TrimSpace(c.User) != c.User {
		return fmt.Errorf("invalid Windows user name %q", c.User)
	}
	if c.Password == "" {
		return fmt.Errorf("a password is required for the Windows administrator account")
	}
	return nil
}

// xmlText escapes s for use as XML character data.
func xmlText(s string) string { return esc(s) }

// unattendXML renders an answer file: the computer name is set in the
// specialize pass; the oobeSystem pass creates a local administrator and
// skips the OOBE screens. All input is XML-escaped.
func (c unattendConfig) unattendXML() (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	const comp = `<component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<unattend xmlns=\"urn:schemas-microsoft-com:unattend\">\n")
	fmt.Fprintf(&b, "  <settings pass=\"specialize\">\n    %s\n      <ComputerName>%s</ComputerName>\n    </component>\n  </settings>\n", comp, xmlText(c.ComputerName))
	fmt.Fprintf(&b, "  <settings pass=\"oobeSystem\">\n    %s\n", comp)
	b.WriteString("      <OOBE>\n        <HideEULAPage>true</HideEULAPage>\n        <HideLocalAccountScreen>true</HideLocalAccountScreen>\n        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>\n        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>\n        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>\n        <ProtectYourPC>3</ProtectYourPC>\n        <SkipMachineOOBE>true</SkipMachineOOBE>\n        <SkipUserOOBE>true</SkipUserOOBE>\n      </OOBE>\n")
	fmt.Fprintf(&b, "      <UserAccounts>\n        <LocalAccounts>\n          <LocalAccount wcm:action=\"add\">\n            <Name>%s</Name>\n            <Group>Administrators</Group>\n            <Password>\n              <Value>%s</Value>\n              <PlainText>true</PlainText>\n            </Password>\n          </LocalAccount>\n        </LocalAccounts>\n      </UserAccounts>\n", xmlText(c.User), xmlText(c.Password))
	b.WriteString("    </component>\n  </settings>\n</unattend>\n")
	return b.String(), nil
}

// image builds the ISO. Windows Setup and first-boot mini-setup look for
// Autounattend.xml / unattend.xml in the root of attached removable media.
func (c unattendConfig) image() ([]byte, error) {
	x, err := c.unattendXML()
	if err != nil {
		return nil, err
	}
	return cloudinit.BuildImage(unattendLabel, map[string][]byte{"Autounattend.xml": []byte(x), "unattend.xml": []byte(x)})
}
