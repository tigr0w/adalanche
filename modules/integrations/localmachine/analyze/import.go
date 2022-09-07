package analyze

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/lkarlslund/adalanche/modules/engine"
	"github.com/lkarlslund/adalanche/modules/integrations/activedirectory"
	"github.com/lkarlslund/adalanche/modules/integrations/localmachine"
	"github.com/lkarlslund/adalanche/modules/ui"
	"github.com/lkarlslund/adalanche/modules/windowssecurity"
)

// Returns the computer object
func ImportCollectorInfo(ao *engine.Objects, cinfo localmachine.Info) (*engine.Object, error) {
	var computerobject *engine.Object
	var existing bool

	domainsid, err := windowssecurity.SIDFromString(cinfo.Machine.ComputerDomainSID)
	if cinfo.Machine.ComputerDomainSID != "" && err == nil {
		computerobject, existing = ao.FindOrAdd(
			activedirectory.ObjectSid, engine.AttributeValueSID(domainsid),
		)
		// It's a duplicate domain member SID :-(
		if existing {
			return nil, fmt.Errorf("duplicate machine info for domain account SID %v found, not loading it. machine names %v and %v", cinfo.Machine.ComputerDomainSID, cinfo.Machine.Name, computerobject.Label())
		}
	}

	if computerobject == nil {
		computerobject = ao.AddNew()
	}

	computerobject.SetFlex(
		engine.IgnoreBlanks,
		activedirectory.SAMAccountName, engine.AttributeValueString(strings.ToUpper(cinfo.Machine.Name)+"$"),
	)

	if cinfo.Machine.WUServer != "" {
		if u, err := url.Parse(cinfo.Machine.WUServer); err == nil {
			host, _, _ := strings.Cut(u.Host, ":")
			computerobject.SetFlex(
				WUServer, engine.AttributeValueString(host),
			)
		}
	}

	if cinfo.Machine.SCCMLastValidMP != "" {
		if u, err := url.Parse(cinfo.Machine.SCCMLastValidMP); err == nil {
			host, _, _ := strings.Cut(u.Host, ":")
			computerobject.SetFlex(
				SCCMServer, engine.AttributeValueString(host),
			)
		}
	}

	var isdomaincontroller bool
	if cinfo.Machine.ProductType != "" {
		// New way of detecting domain controller
		isdomaincontroller = strings.EqualFold(cinfo.Machine.ProductType, "SERVERNT")
	} else {
		// OK, lets brute force this alien
		for _, group := range cinfo.Groups {
			if group.SID == "S-1-5-32-548" {
				// Account Operators exists only locally on DCs
				isdomaincontroller = true
				break
			}
		}
	}

	downlevelmachinename := cinfo.Machine.Domain + "\\" + cinfo.Machine.Name + "$"

	// Local accounts should not merge, unless we're a DC, then it's OK to merge with the domain source
	uniquesource := cinfo.Machine.Name
	if isdomaincontroller {
		uniquesource = cinfo.Machine.Domain
	}

	// Don't set UniqueSource on the computer object, it needs to merge with the AD object!
	// computerobject.SetFlex(engine.UniqueSource, uniquesource)

	if cinfo.Machine.IsDomainJoined {
		computerobject.SetValues(engine.DownLevelLogonName, engine.AttributeValueString(downlevelmachinename))
	}

	// See if the machine has a unique SID
	localsid, err := windowssecurity.SIDFromString(cinfo.Machine.LocalSID)
	if err != nil {
		return nil, fmt.Errorf("collected localmachine information for %v doesn't contain valid local machine SID (%v): %v", cinfo.Machine.Name, cinfo.Machine.LocalSID, err)
	}

	macaddrs := engine.AttributeValueSlice{}
	for _, networkinterface := range cinfo.Network.NetworkInterfaces {
		if strings.Count(networkinterface.MACAddress, ":") == 5 {
			// Sanity check above removes ISATAP interfaces

			if strings.EqualFold(networkinterface.MACAddress, "02:00:4c:4f:4f:50") {
				// Loopback adapter, skip it
				continue
			}

			if strings.EqualFold(networkinterface.MACAddress, "02:50:41:00:00:01") {
				// Palo Alto Protect network interface
				continue
			}

			macaddrs = append(macaddrs, engine.AttributeValueString(strings.ReplaceAll(networkinterface.MACAddress, ":", "")))
		}
	}
	if len(macaddrs) > 0 {
		computerobject.SetValues(localmachine.MACAddress, macaddrs...)
	}

	ao.ReindexObject(computerobject, false) // We changed stuff after adding it

	// Add local accounts as synthetic objects
	userscontainer := engine.NewObject(activedirectory.Name, "Users")
	ao.Add(userscontainer)
	userscontainer.ChildOf(computerobject)

	if !isdomaincontroller {
		for _, user := range cinfo.Users {
			uac := 512
			if !user.IsEnabled {
				uac += 2
			}
			if user.IsLocked {
				uac += 16
			}
			if user.NoChangePassword {
				uac += 0x10000
			}
			usid, err := windowssecurity.SIDFromString(user.SID)
			if err == nil {
				if domainsid.StripRID() == usid.StripRID() {
					// Domain user from a DC, just drop it silently, we got this from the AD dump
					continue
				}

				// Potential translation
				// usid = MapSID(originalsid, localsid, usid)

				user := ao.AddNew(
					engine.IgnoreBlanks,
					activedirectory.ObjectSid, engine.AttributeValueSID(usid),
					activedirectory.ObjectCategorySimple, "Person",
					activedirectory.DisplayName, user.FullName,
					activedirectory.Name, user.Name,
					activedirectory.UserAccountControl, uac,
					activedirectory.PwdLastSet, user.PasswordLastSet,
					activedirectory.LastLogon, user.LastLogon,
					engine.DownLevelLogonName, cinfo.Machine.Name+"\\"+user.Name,
					activedirectory.BadPwdCount, user.BadPasswordCount,
					activedirectory.LogonCount, user.NumberOfLogins,
					engine.UniqueSource, uniquesource,
				)
				user.ChildOf(userscontainer)
			} else {
				ui.Warn().Msgf("Invalid user SID in dump: %v", user.SID)
			}
		}

		// Iterate over Groups
		groupscontainer := engine.NewObject(activedirectory.Name, "Groups")
		ao.Add(groupscontainer)
		groupscontainer.ChildOf(computerobject)
		for _, group := range cinfo.Groups {
			groupsid, err := windowssecurity.SIDFromString(group.SID)
			// Potential translation
			// groupsid = MapSID(originalsid, localsid, groupsid)

			groupobject := ao.AddNew(
				activedirectory.ObjectSid, engine.AttributeValueSID(groupsid),
				activedirectory.Name, group.Name,
				activedirectory.Description, group.Comment,
				engine.ObjectCategorySimple, "Group",
				engine.UniqueSource, uniquesource,
			)

			if err != nil && group.Name != "SMS Admins" {
				ui.Warn().Msgf("Can't convert local group SID %v: %v", group.SID, err)
				continue
			}
			for _, member := range group.Members {
				var membersid windowssecurity.SID
				if member.SID != "" {
					membersid, err = windowssecurity.SIDFromString(member.SID)
					if err != nil {
						ui.Warn().Msgf("Can't convert local group member SID %v: %v", member.SID, err)
						continue
					}
				} else {
					// Some members show up with the SID in the name field FML
					membersid, err = windowssecurity.SIDFromString(member.Name)
					if err != nil {
						ui.Info().Msgf("Fallback SID translation on %v failed: %v", member.Name, err)
						continue
					}
				}

				if membersid.Component(2) != 21 {
					continue // Not a local or domain SID, skip it
				}

				if membersid.Components() != 7 {
					ui.Warn().Msgf("Malformed SID from collector: %v, skipping member entry entirely", membersid.String())
					continue
				}

				// Collector sometimes returns junk, remove it
				if strings.HasSuffix(member.Name, "\\") || strings.HasPrefix(member.Name, "S-1-") {
					// If name resolution fails, you end up with DOMAIN\ and nothing else
					member.Name = ""
				}

				// Potential translation
				// membersid = MapSID(originalsid, localsid, membersid)

				memberobject := ao.AddNew(
					activedirectory.ObjectSid, engine.AttributeValueSID(membersid),
					engine.IgnoreBlanks,
					engine.DownLevelLogonName, member.Name,
				)

				if membersid.StripRID() == localsid || (membersid.Component(2) != 21 && membersid != windowssecurity.EveryoneSID && membersid != windowssecurity.AuthenticatedUsersSID) {
					memberobject.SetFlex(
						engine.UniqueSource, uniquesource,
					)
				}

				memberobject.Pwns(groupobject, activedirectory.PwnMemberOfGroup)

				switch {
				case group.Name == "SMS Admins":
					memberobject.Pwns(computerobject, PwnLocalSMSAdmins)
				case groupsid == windowssecurity.AdministratorsSID:
					memberobject.Pwns(computerobject, PwnLocalAdminRights)
				case groupsid == windowssecurity.DCOMUsersSID:
					memberobject.Pwns(computerobject, PwnLocalDCOMRights)
				case groupsid == windowssecurity.RemoteDesktopUsersSID:
					memberobject.Pwns(computerobject, PwnLocalRDPRights)
				}

				if membersid.StripRID() == localsid || membersid.Component(2) != 21 {
					// Local user or group, we don't know - add it to computer for now
					memberobject.ChildOf(computerobject)
				}
			}
		}
	}

	// USERS THAT HAVE SESSIONS ON THE MACHINE ONCE IN WHILE
	for _, login := range cinfo.LoginPopularity.Day {
		usersid, err := windowssecurity.SIDFromString(login.SID)
		if err != nil {
			ui.Warn().Msgf("Can't convert local user SID %v: %v", login.SID, err)
			continue
		}
		if usersid.Component(2) != 21 {
			continue // Not a local or domain SID, skip it
		}

		// Potential translation
		// usersid = MapSID(originalsid, localsid, usersid)

		user := ao.AddNew(
			activedirectory.ObjectSid, engine.AttributeValueSID(usersid),
			engine.ObjectCategorySimple, "Person",
		)
		if usersid.StripRID() == localsid || usersid.Component(2) != 21 {
			user.SetFlex(
				engine.UniqueSource, uniquesource,
			)
		}

		if !strings.HasSuffix(login.Name, "\\") {
			user.SetValues(engine.DownLevelLogonName, engine.AttributeValueString(login.Name))
		}

		computerobject.Pwns(user, PwnLocalSessionLastDay)
	}

	for _, login := range cinfo.LoginPopularity.Week {
		usersid, err := windowssecurity.SIDFromString(login.SID)
		if err != nil {
			ui.Warn().Msgf("Can't convert local user SID %v: %v", login.SID, err)
			continue
		}
		if usersid.Component(2) != 21 {
			continue // Not a domain SID, skip it
		}

		// Potential translation
		// usersid = MapSID(originalsid, localsid, usersid)

		user := ao.AddNew(
			activedirectory.ObjectSid, engine.AttributeValueSID(usersid),
		)
		if usersid.StripRID() == localsid || usersid.Component(2) != 21 {
			user.SetFlex(
				engine.UniqueSource, uniquesource,
			)
		}

		if !strings.HasSuffix(login.Name, "\\") {
			user.SetValues(engine.DownLevelLogonName, engine.AttributeValueString(login.Name))
		}

		computerobject.Pwns(user, PwnLocalSessionLastWeek)
	}

	for _, login := range cinfo.LoginPopularity.Month {
		usersid, err := windowssecurity.SIDFromString(login.SID)
		if err != nil {
			ui.Warn().Msgf("Can't convert local user SID %v: %v", login.SID, err)
			continue
		}
		if usersid.Component(2) != 21 {
			continue // Not a domain SID, skip it
		}

		// Potential translation
		// usersid = MapSID(originalsid, localsid, usersid)

		user := ao.AddNew(
			activedirectory.ObjectSid, engine.AttributeValueSID(usersid),
		)
		if usersid.StripRID() == localsid || usersid.Component(2) != 21 {
			user.SetFlex(
				engine.UniqueSource, uniquesource,
			)
		}

		if !strings.HasSuffix(login.Name, "\\") {
			user.SetValues(engine.DownLevelLogonName, engine.AttributeValueString(login.Name))
		}

		computerobject.Pwns(user, PwnLocalSessionLastMonth)
	}

	// AUTOLOGIN CREDENTIALS - ONLY IF DOMAIN JOINED AND IT'S TO THIS DOMAIN
	if cinfo.Machine.DefaultUsername != "" &&
		cinfo.Machine.DefaultDomain != "" &&
		cinfo.Machine.DefaultDomain == cinfo.Machine.Domain {
		// NETBIOS name for domain check FIXME
		user, _ := ao.FindOrAdd(
			engine.NetbiosDomain, engine.AttributeValueString(cinfo.Machine.DefaultDomain),
			activedirectory.SAMAccountName, cinfo.Machine.DefaultUsername,
			engine.DownLevelLogonName, cinfo.Machine.DefaultDomain+"\\"+cinfo.Machine.DefaultUsername,
			activedirectory.ObjectCategorySimple, "Person",
		)
		computerobject.Pwns(user, PwnHasAutoAdminLogonCredentials)
	}

	// SERVICES
	servicescontainer := engine.NewObject(activedirectory.Name, "Services")
	ao.Add(servicescontainer)
	servicescontainer.ChildOf(computerobject)

	localservicesgroup := ao.AddNew(
		activedirectory.ObjectSid, engine.AttributeValueSID(windowssecurity.LocalServiceSID),
		engine.DownLevelLogonName, cinfo.Machine.Name+"\\Services",
		engine.UniqueSource, cinfo.Machine.Name,
	)

	for _, service := range cinfo.Services {
		serviceobject := engine.NewObject(
			engine.IgnoreBlanks,
			activedirectory.Name, service.Name,
			activedirectory.DisplayName, service.Name,
			activedirectory.Description, service.Description,
			ServiceStart, int64(service.Start),
			ServiceType, int64(service.Type),
			activedirectory.ObjectCategorySimple, "Service",
		)
		ao.Add(serviceobject)
		serviceobject.ChildOf(servicescontainer)
		serviceobject.Pwns(localservicesgroup, engine.PwnMemberOfGroup)
		computerobject.Pwns(serviceobject, PwnHosts)

		if serviceaccountSID, err := windowssecurity.SIDFromString(service.AccountSID); err == nil && serviceaccountSID.Component(2) == 21 {

			// Potential translation
			// serviceaccountSID = MapSID(originalsid, localsid, serviceaccountSID)

			nameparts := strings.Split(service.Account, "\\")
			if len(nameparts) == 2 && nameparts[0] != cinfo.Machine.Domain { // FIXME - NETBIOS NAMES ARE KILLIG US
				svcaccount, _ := ao.FindOrAdd(
					activedirectory.ObjectSid, engine.AttributeValueSID(serviceaccountSID),
					engine.IgnoreBlanks,
					engine.DownLevelLogonName, service.Account,
					// activedirectory.SAMAccountName, engine.AttributeValueString(nameparts[1]),
					// activedirectory.ObjectCategorySimple, engine.AttributeValueString("Person"),
				)
				if serviceaccountSID.StripRID() == localsid || serviceaccountSID.Component(2) != 21 {
					svcaccount.SetFlex(
						engine.UniqueSource, uniquesource,
					)
				}

				computerobject.Pwns(svcaccount, PwnHasServiceAccountCredentials)
				serviceobject.Pwns(svcaccount, PwnRunsAs)
			}
		} else if strings.EqualFold(service.Account, "LocalSystem") {
			serviceobject.Pwns(computerobject, PwnRunsAs)
		}

		// Change service executable via registry
		if sd, err := engine.ParseACL(service.RegistryDACL); err == nil {
			for _, entry := range sd.Entries {
				entrysid := entry.SID
				if entry.Type == engine.ACETYPE_ACCESS_ALLOWED {
					if entrysid == windowssecurity.AdministratorsSID || entrysid == windowssecurity.SystemSID || entrysid.Component(2) == 80 /* Service user */ {
						// if we have local admin it's already game over so don't map this
						continue
					}

					o := ao.AddNew(
						activedirectory.ObjectSid, engine.AttributeValueSID(entrysid),
					)

					if entrysid != windowssecurity.EveryoneSID && (entrysid.StripRID() == localsid || entrysid.Component(2) != 21) {
						o.SetFlex(
							engine.UniqueSource, uniquesource,
						)
					}

					if entry.Mask&engine.KEY_SET_VALUE == engine.KEY_SET_VALUE {
						o.Pwns(serviceobject, PwnRegistryWrite)
					}

					if entry.Mask&engine.RIGHT_WRITE_DACL == engine.RIGHT_WRITE_DACL {
						o.Pwns(serviceobject, PwnRegistryModifyDACL)
					}
				}
			}
		}

		// Change service executable contents
		serviceimageobject := engine.NewObject(
			activedirectory.DisplayName, filepath.Base(service.ImageExecutable),
			AbsolutePath, service.ImageExecutable,
			engine.ObjectCategorySimple, "Executable",
		)
		ao.Add(serviceimageobject)
		serviceimageobject.Pwns(serviceobject, PwnExecuted)
		serviceimageobject.ChildOf(serviceobject)

		if ownersid, err := windowssecurity.SIDFromString(service.ImageExecutableOwner); err == nil {
			// Potential translation
			if ownersid.Component(2) == 80 /* Service user */ {
				continue
			}

			owner := ao.AddNew(
				activedirectory.ObjectSid, engine.AttributeValueSID(ownersid),
			)
			if ownersid.StripRID() == localsid || ownersid.Component(2) != 21 {
				owner.SetFlex(
					engine.UniqueSource, uniquesource,
				)
			}
			owner.Pwns(serviceimageobject, PwnFileOwner)
		}

		if sd, err := engine.ParseACL(service.ImageExecutableDACL); err == nil {
			for _, entry := range sd.Entries {
				entrysid := entry.SID
				if entry.Type == engine.ACETYPE_ACCESS_ALLOWED && (entrysid.Component(2) == 21 || entry.SID == windowssecurity.EveryoneSID || entry.SID == windowssecurity.AuthenticatedUsersSID) {
					o := ao.AddNew(
						activedirectory.ObjectSid, engine.AttributeValueSID(entrysid),
					)
					if entrysid.StripRID() == localsid || entrysid.Component(2) != 21 {
						o.SetFlex(
							engine.UniqueSource, uniquesource,
						)
					}

					if entry.Mask&engine.FILE_WRITE_DATA != 0 {
						o.Pwns(serviceimageobject, PwnFileWrite)
					}
					if entry.Mask&engine.RIGHT_WRITE_OWNER != 0 {
						o.Pwns(serviceimageobject, PwnFileTakeOwnership) // Not sure about this one
					}
					if entry.Mask&engine.RIGHT_WRITE_DACL != 0 {
						o.Pwns(serviceimageobject, PwnFileModifyDACL)
					}
				}
			}
			// ui.Debug().Msgf("Service %v executable %v: %v", service.Name, service.ImageExecutable, sd)
		}
	}

	// SOFTWARE INVENTORY AS ATTRIBUTES
	installedsoftware := make([]string, len(cinfo.Software))
	for i, software := range cinfo.Software {
		installedsoftware[i] = fmt.Sprintf(
			"%v %v %v", software.Publisher, software.DisplayName, software.DisplayVersion,
		)
	}
	if len(installedsoftware) > 0 {
		computerobject.SetFlex(localmachine.InstalledSoftware, installedsoftware)
	}

	// Privileges to exploits - from https://github.com/gtworek/Priv2Admin
	for _, pi := range cinfo.Privileges {
		var pwn engine.Edge
		switch pi.Name {
		case "SeBackupPrivilege":
			pwn = PwnSeBackupPrivilege
		case "SeRestorePrivilege":
			pwn = PwnSeRestorePrivilege
		case "SeAssignPrimaryTokenPrivilege":
			pwn = PwnSeAssignPrimaryToken
		case "SeCreateTokenPrivilege":
			pwn = PwnSeCreateToken
		case "SeDebugPrivilege":
			pwn = PwnSeDebug
		case "SeImpersonatePrivilege":
			pwn = PwnSeImpersonate
		case "SeLoadDriverPrivilege":
			pwn = PwnSeLoadDriver
		case "SeManageVolumePrivilege":
			pwn = PwnSeManageVolume
		case "SeTakeOwnershipPrivilege":
			pwn = PwnSeTakeOwnership
		case "SeTcbPrivilege":
			pwn = PwnSeTcb
		default:
			continue
		}

		for _, sidstring := range pi.AssignedSIDs {
			sid, err := windowssecurity.SIDFromString(sidstring)
			if err != nil {
				ui.Error().Msgf("Invalid SID %v: %v", sidstring, err)
				continue
			}

			// Only domain users for now
			if sid.Component(2) != 21 && sid != windowssecurity.LocalServiceSID && sid != windowssecurity.NetworkServiceSID && sid != windowssecurity.ServicesSID {
				continue
			}

			// Potential translation
			// sid = MapSID(originalsid, localsid, sid)
			assignee := ao.AddNew(
				activedirectory.ObjectSid, engine.AttributeValueSID(sid),
			)
			if sid.StripRID() == localsid || sid.Component(2) != 21 {
				assignee.SetFlex(
					engine.UniqueSource, uniquesource,
				)
			}

			assignee.Pwns(computerobject, pwn)
		}
	}

	// SHARES
	if len(cinfo.Shares) > 0 {
		computershares := ao.AddNew(
			activedirectory.ObjectCategorySimple, "Container",
			activedirectory.DisplayName, "Shares",
		)
		computershares.ChildOf(computerobject)

		for _, share := range cinfo.Shares {
			shareobject := ao.AddNew(
				engine.IgnoreBlanks,
				activedirectory.DisplayName, "\\\\"+cinfo.Machine.Name+"\\"+share.Name,
				AbsolutePath, share.Path,
				engine.Description, share.Remark,
				ShareType, share.Type,
				engine.ObjectCategorySimple, "Share",
			)

			computerobject.Pwns(shareobject, PwnFileShare)

			shareobject.ChildOf(computershares)

			if sd, err := engine.ParseSecurityDescriptor(share.DACL); err == nil {
				// if !sd.Owner.IsNull() {
				// 	ui.Warn().Msgf("Share %v has owner set to %v", share.Name, sd.Owner)
				// }
				// if !sd.Group.IsNull() {
				// 	ui.Warn().Msgf("Share %v has group set to %v", share.Name, sd.Group)
				// }
				for _, entry := range sd.DACL.Entries {
					entrysid := entry.SID
					if entry.Type == engine.ACETYPE_ACCESS_ALLOWED {
						o := ao.AddNew(
							activedirectory.ObjectSid, engine.AttributeValueSID(entrysid),
						)
						if entrysid.StripRID() == localsid || entrysid.Component(2) != 21 {
							o.SetFlex(
								engine.UniqueSource, uniquesource,
							)
						}
						if entry.Mask&engine.FILE_READ_DATA != 0 {
							o.Pwns(shareobject, PwnFileRead)
						}
						if entry.Mask&engine.FILE_WRITE_DATA != 0 {
							o.Pwns(shareobject, PwnFileWrite)
						}
						if entry.Mask&engine.RIGHT_WRITE_OWNER != 0 {
							o.Pwns(shareobject, PwnFileTakeOwnership) // Not sure about this one
						}
						if entry.Mask&engine.RIGHT_WRITE_DACL != 0 {
							o.Pwns(shareobject, PwnFileModifyDACL)
						}
					}
				}
			}
		}
	}

	// Everyone / World and Authenticated Users merge with Domain - not pretty IMO
	if cinfo.Machine.IsDomainJoined && !isdomaincontroller {
		domaineveryoneobject := ao.AddNew(
			activedirectory.ObjectSid, engine.AttributeValueSID(windowssecurity.EveryoneSID),
			engine.UniqueSource, engine.AttributeValueString(cinfo.Machine.Domain),
		)

		if everyone, found := ao.FindTwoMulti(engine.ObjectSid, engine.AttributeValueSID(windowssecurity.EveryoneSID),
			engine.UniqueSource, engine.AttributeValueString(uniquesource)); found {
			for _, o := range everyone {
				domaineveryoneobject.Pwns(o, activedirectory.PwnMemberOfGroup)
			}
		}

		domainauthenticatedusers := ao.AddNew(
			activedirectory.ObjectSid, engine.AttributeValueSID(windowssecurity.AuthenticatedUsersSID),
			engine.UniqueSource, engine.AttributeValueString(cinfo.Machine.Domain),
		)

		if authenticatedusers, found := ao.FindTwoMulti(engine.ObjectSid, engine.AttributeValueSID(windowssecurity.AuthenticatedUsersSID),
			engine.UniqueSource, engine.AttributeValueString(uniquesource)); found {
			for _, o := range authenticatedusers {
				domainauthenticatedusers.Pwns(o, activedirectory.PwnMemberOfGroup)
			}
		}
	}

	return computerobject, nil
}