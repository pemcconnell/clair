// Copyright 2019 clair authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package amzn implements a vulnerability source updater using
// ALAS (Amazon Linux Security Advisories).
package amzn

import (
	"bufio"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/coreos/clair/database"
	"github.com/coreos/clair/ext/versionfmt"
	"github.com/coreos/clair/ext/versionfmt/rpm"
	"github.com/coreos/clair/ext/vulnsrc"
	"github.com/coreos/clair/pkg/commonerr"
	"github.com/coreos/clair/pkg/httputil"
)

var (
	// This will be overwritten by os.GetEnv("VULNSRC_AMZN1_MIRROR") if present
	amazonLinux1MirrorListURI = "http://repo.us-west-2.amazonaws.com/2018.03/updates/x86_64/mirror.list"
	// This will be overwritten by os.GetEnv("VULNSRC_AMZN2_MIRROR") if present
	amazonLinux2MirrorListURI = "https://cdn.amazonlinux.com/2/core/latest/x86_64/mirror.list"
)

const (
	amazonLinux1UpdaterFlag = "amazonLinux1Updater"
	amazonLinux1Name        = "Amazon Linux 2018.03"
	amazonLinux1Namespace   = "amzn:2018.03"
	amazonLinux1LinkFormat  = "https://alas.aws.amazon.com/%s.html"

	amazonLinux2UpdaterFlag = "amazonLinux2Updater"
	amazonLinux2Name        = "Amazon Linux 2"
	amazonLinux2Namespace   = "amzn:2"
	amazonLinux2LinkFormat  = "https://alas.aws.amazon.com/AL2/%s.html"
)

type updater struct {
	UpdaterFlag   string
	MirrorListURI string
	Name          string
	Namespace     string
	LinkFormat    string
}

func init() {
	// optional overrides
	if os.Getenv("VULNSRC_AMZN1_MIRROR") != "" {
		amazonLinux1MirrorListURI = os.Getenv("VULNSRC_AMZN1_MIRROR")
	}
	if os.Getenv("VULNSRC_AMZN2_MIRROR") != "" {
		amazonLinux2MirrorListURI = os.Getenv("VULNSRC_AMZN2_MIRROR")
	}
	// Register updater for Amazon Linux 2018.03.
	amazonLinux1Updater := updater{
		UpdaterFlag:   amazonLinux1UpdaterFlag,
		MirrorListURI: amazonLinux1MirrorListURI,
		Name:          amazonLinux1Name,
		Namespace:     amazonLinux1Namespace,
		LinkFormat:    amazonLinux1LinkFormat,
	}
	vulnsrc.RegisterUpdater("amzn1", &amazonLinux1Updater)

	// Register updater for Amazon Linux 2.
	amazonLinux2Updater := updater{
		UpdaterFlag:   amazonLinux2UpdaterFlag,
		MirrorListURI: amazonLinux2MirrorListURI,
		Name:          amazonLinux2Name,
		Namespace:     amazonLinux2Namespace,
		LinkFormat:    amazonLinux2LinkFormat,
	}
	vulnsrc.RegisterUpdater("amzn2", &amazonLinux2Updater)
}

func (u *updater) Update(datastore database.Datastore) (vulnsrc.UpdateResponse, error) {
	log.WithField("package", u.Name).Info("Start fetching vulnerabilities")

	// Get the flag value (the timestamp of the latest ALAS of the previous update).
	flagValue, found, err := database.FindKeyValueAndRollback(datastore, u.UpdaterFlag)
	if err != nil {
		return vulnsrc.UpdateResponse{}, err
	}

	if !found {
		flagValue = ""
	}

	var timestamp string

	// Get the ALASs from updateinfo.xml.gz from the repos.
	updateInfo, err := u.getUpdateInfo()
	if err != nil {
		return vulnsrc.UpdateResponse{}, err
	}

	// Get the ALASs which were issued/updated since the previous update.
	var alasList []ALAS
	for _, alas := range updateInfo.ALASList {
		if compareTimestamp(alas.Updated.Date, flagValue) > 0 {
			alasList = append(alasList, alas)

			if compareTimestamp(alas.Updated.Date, timestamp) > 0 {
				timestamp = alas.Updated.Date
			}
		}
	}

	// Get the vulnerabilities.
	vulnerabilities := u.alasListToVulnerabilities(alasList)

	response := vulnsrc.UpdateResponse{
		Vulnerabilities: vulnerabilities,
	}

	// Set the flag value.
	if timestamp != "" {
		response.FlagName = u.UpdaterFlag
		response.FlagValue = timestamp
	} else {
		log.WithField("package", u.Name).Debug("no update")
	}

	return response, err
}

func (u *updater) Clean() {

}

func (u *updater) getUpdateInfo() (UpdateInfo, error) {
	// Get the URI of updateinfo.xml.gz.
	updateInfoURI, err := u.getUpdateInfoURI()
	if err != nil {
		return UpdateInfo{}, err
	}

	// Download updateinfo.xml.gz.
	updateInfoResponse, err := httputil.GetWithUserAgent(updateInfoURI)
	if err != nil {
		log.WithError(err).Error("could not download updateinfo.xml.gz")
		return UpdateInfo{}, commonerr.ErrCouldNotDownload
	}
	defer updateInfoResponse.Body.Close()

	if !httputil.Status2xx(updateInfoResponse) {
		log.WithField("StatusCode", updateInfoResponse.StatusCode).Error("could not download updateinfo.xml.gz")
		return UpdateInfo{}, commonerr.ErrCouldNotDownload
	}

	// Decompress updateinfo.xml.gz.
	updateInfoXml, err := gzip.NewReader(updateInfoResponse.Body)
	if err != nil {
		log.WithError(err).Error("could not decompress updateinfo.xml.gz")
		return UpdateInfo{}, commonerr.ErrCouldNotParse
	}
	defer updateInfoXml.Close()

	// Decode updateinfo.xml.
	updateInfo, err := decodeUpdateInfo(updateInfoXml)
	if err != nil {
		log.WithError(err).Error("could not decode updateinfo.xml")
		return UpdateInfo{}, commonerr.ErrCouldNotParse
	}

	return updateInfo, nil
}

func (u *updater) getUpdateInfoURI() (string, error) {
	// Download mirror.list
	mirrorListResponse, err := httputil.GetWithUserAgent(u.MirrorListURI)
	if err != nil {
		log.WithError(err).Error("could not download mirror list")
		return "", commonerr.ErrCouldNotDownload
	}
	defer mirrorListResponse.Body.Close()

	if !httputil.Status2xx(mirrorListResponse) {
		log.WithField("StatusCode", mirrorListResponse.StatusCode).Error("could not download mirror list")
		return "", commonerr.ErrCouldNotDownload
	}

	// Parse the URI of the first mirror.
	scanner := bufio.NewScanner(mirrorListResponse.Body)
	success := scanner.Scan()
	if success != true {
		log.WithError(err).Error("could not parse mirror list")
	}
	mirrorURI := scanner.Text()

	// Download repomd.xml.
	repoMdURI := mirrorURI + "/repodata/repomd.xml"
	repoMdResponse, err := httputil.GetWithUserAgent(repoMdURI)
	if err != nil {
		log.WithError(err).Error("could not download repomd.xml")
		return "", commonerr.ErrCouldNotDownload
	}
	defer repoMdResponse.Body.Close()

	if !httputil.Status2xx(repoMdResponse) {
		log.WithField("StatusCode", repoMdResponse.StatusCode).Error("could not download repomd.xml")
		return "", commonerr.ErrCouldNotDownload
	}

	// Decode repomd.xml.
	var repoMd RepoMd
	err = xml.NewDecoder(repoMdResponse.Body).Decode(&repoMd)
	if err != nil {
		log.WithError(err).Error("could not decode repomd.xml")
		return "", commonerr.ErrCouldNotDownload
	}

	// Parse the URI of updateinfo.xml.gz.
	var updateInfoURI string
	for _, repo := range repoMd.RepoList {
		if repo.Type == "updateinfo" {
			updateInfoURI = mirrorURI + "/" + repo.Location.Href
			break
		}
	}
	if updateInfoURI == "" {
		log.Error("could not find updateinfo in repomd.xml")
		return "", commonerr.ErrCouldNotDownload
	}

	return updateInfoURI, nil
}

func decodeUpdateInfo(updateInfoReader io.Reader) (UpdateInfo, error) {
	var updateInfo UpdateInfo
	err := xml.NewDecoder(updateInfoReader).Decode(&updateInfo)
	if err != nil {
		return updateInfo, err
	}

	return updateInfo, nil
}

func (u *updater) alasListToVulnerabilities(alasList []ALAS) []database.VulnerabilityWithAffected {
	var vulnerabilities []database.VulnerabilityWithAffected
	for _, alas := range alasList {
		featureVersions := u.alasToFeatureVersions(alas)
		if len(featureVersions) > 0 {
			vulnerability := database.VulnerabilityWithAffected{
				Vulnerability: database.Vulnerability{
					Name:        u.alasToName(alas),
					Link:        u.alasToLink(alas),
					Severity:    u.alasToSeverity(alas),
					Description: u.alasToDescription(alas),
				},
				Affected: featureVersions,
			}
			vulnerabilities = append(vulnerabilities, vulnerability)
		}
	}

	return vulnerabilities
}

func (u *updater) alasToName(alas ALAS) string {
	return alas.Id
}

func (u *updater) alasToLink(alas ALAS) string {
	if u.Name == amazonLinux1Name {
		return fmt.Sprintf(u.LinkFormat, alas.Id)
	}

	if u.Name == amazonLinux2Name {
		// "ALAS2-2018-1097" becomes "https://alas.aws.amazon.com/AL2/ALAS-2018-1097.html".
		re := regexp.MustCompile(`^ALAS2-(.+)$`)
		return fmt.Sprintf(u.LinkFormat, "ALAS-"+re.FindStringSubmatch(alas.Id)[1])
	}

	return ""
}

func (u *updater) alasToSeverity(alas ALAS) database.Severity {
	switch alas.Severity {
	case "low":
		return database.LowSeverity
	case "medium":
		return database.MediumSeverity
	case "important":
		return database.HighSeverity
	case "critical":
		return database.CriticalSeverity
	default:
		log.WithField("severity", alas.Severity).Warning("could not determine vulnerability severity")
		return database.UnknownSeverity
	}
}

func (u *updater) alasToDescription(alas ALAS) string {
	re := regexp.MustCompile(`\s+`)
	return re.ReplaceAllString(strings.TrimSpace(alas.Description), " ")
}

func (u *updater) alasToFeatureVersions(alas ALAS) []database.AffectedFeature {
	var featureVersions []database.AffectedFeature
	for _, p := range alas.Packages {
		var version string
		if p.Epoch == "0" {
			version = p.Version + "-" + p.Release
		} else {
			version = p.Epoch + ":" + p.Version + "-" + p.Release
		}
		err := versionfmt.Valid(rpm.ParserName, version)
		if err != nil {
			log.WithError(err).WithField("version", version).Warning("could not parse package version. skipping")
			continue
		}

		featureVersion := database.AffectedFeature{
			Namespace: database.Namespace{
				Name:          u.Namespace,
				VersionFormat: rpm.ParserName,
			},
			FeatureName:     p.Name,
			AffectedVersion: version,
			FeatureType:     database.BinaryPackage,
		}

		if version != versionfmt.MaxVersion {
			featureVersion.FixedInVersion = version
		}

		featureVersions = append(featureVersions, featureVersion)
	}

	return featureVersions
}

func compareTimestamp(date0 string, date1 string) int {
	// format: YYYY-MM-DD hh:mm
	if date0 < date1 {
		return -1
	} else if date0 > date1 {
		return 1
	} else {
		return 0
	}
}
