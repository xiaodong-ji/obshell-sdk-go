/*
 * Copyright (c) 2024 OceanBase.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package util

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/go-resty/resty/v2"

	"github.com/oceanbase/obshell-sdk-go/internal/util"
)

const (
	REMOTE_REPOMD_FILE  = "/repodata/repomd.xml"
	REPO_AGE_FILE       = ".rege_age"
	PRIMARY_REPOMD_TYPE = "primary"
)

type Mirror struct {
	name   string
	url    string
	arch   string
	nonLse bool
}

type baseMirror struct {
	name    string
	baseUrl string
}

func NewBaseMirror(name, baseUrl string) *baseMirror {
	return &baseMirror{
		name:    name,
		baseUrl: baseUrl,
	}
}

func (bm *baseMirror) GetMirror(arch, release string, nonLse ...bool) Mirror {
	url := strings.Replace(bm.baseUrl, "$releasever", release, -1)
	url = strings.Replace(url, "$basearch", arch, -1)

	name := strings.Replace(bm.name, "$releasever", release, -1)
	name = strings.Replace(name, "$basearch", arch, -1)

	nonLse = append(nonLse, !supoortLse)
	return Mirror{
		name:   name,
		url:    url,
		arch:   arch,
		nonLse: nonLse[0],
	}
}

type repoMD struct {
	XMLName  xml.Name   `xml:"repomd"`
	Revision string     `xml:"revision"`
	Data     []repoData `xml:"data"`
}

type repoData struct {
	Type            string   `xml:"type,attr"`
	Location        Location `xml:"location"`
	Timestamp       int      `xml:"timestamp"`
	Size            int      `xml:"size"`
	OpenSize        int      `xml:"open-size"`
	DatabaseVersion int      `xml:"database_version,omitempty"` // Optional field
}

type primaryData struct {
	Packages []packageInfo `xml:"package"`
}

type packageInfo struct {
	XMLName  xml.Name       `xml:"package"`
	Name     string         `xml:"name"`
	Arch     string         `xml:"arch"`
	Version  packageVersion `xml:"version"`
	Packager string         `xml:"packager"`
	URL      string         `xml:"url"`
	Time     packageTime    `xml:"time"`
	Size     packageSize    `xml:"size"`
	Location Location       `xml:"location"`
	Format   packageFormat  `xml:"format"`
}

type packageVersion struct {
	Epoch   string `xml:"epoch,attr"`
	Version string `xml:"ver,attr"`
	Release string `xml:"rel,attr"`
}

type packageTime struct {
	File  string `xml:"file,attr"`
	Build string `xml:"build,attr"`
}

type packageSize struct {
	Package   string `xml:"package,attr"`
	Installed string `xml:"installed,attr"`
	Archive   string `xml:"archive,attr"`
}

type Location struct {
	BaseUrl string `xml:"base,attr"`
	Href    string `xml:"href,attr"`
}

type packageFormat struct {
	License     string               `xml:"rpm:license"`
	Vendor      string               `xml:"rpm:vendor"`
	Group       string               `xml:"rpm:group"`
	BuildHost   string               `xml:"rpm:buildhost"`
	SourceRPM   string               `xml:"rpm:sourcerpm"`
	HeaderRange packageHeaderRange   `xml:"rpm:header-range"`
	Provides    []PackageEntry       `xml:"rpm:provides>rpm:entry"`
	Requires    []PackageEntry       `xml:"rpm:requires>rpm:entry"`
	Files       []packageIncludeFile `xml:"file"`
}

type packageHeaderRange struct {
	Start int `xml:"start,attr"`
	End   int `xml:"end,attr"`
}

type PackageEntry struct {
	Name    string `xml:"name,attr"`
	Flags   string `xml:"flags,attr,omitempty"`
	Epoch   string `xml:"epoch,attr,omitempty"`
	Version string `xml:"ver,attr,omitempty"`
	Release string `xml:"rel,attr,omitempty"`
}

type packageIncludeFile struct {
	Type string `xml:"type,attr,omitempty"`
	Path string `xml:",chardata"`
}

const (
	X86_64  = "x86_64"
	AARCH64 = "aarch64"
	NONLSE  = "nonlse"

	EL7 = "7"
	EL8 = "8"
)

var (
	arch       string
	release    string
	supoortLse bool

	architectureMap = map[string]string{
		"amd64": X86_64,
		"arm64": AARCH64,
	}

	// Base mirrors
	OB_COMMUNITY_STABLE_BASE = NewBaseMirror("OceanBase-community-stable-el$releasever", "https://mirrors.oceanbase.com/oceanbase/community/stable/el/$releasever/$basearch/")
	OB_DEVELOPMENT_KIT_BASE  = NewBaseMirror("OceanBase-development-kit-el$releasever", "https://mirrors.oceanbase.com/oceanbase/development-kit/el/$releasever/$basearch/")

	// Mirrors
	OB_COMMUNITY_STABLE_MIRROR Mirror
	OB_DEVELOPMENT_KIT_MIRROR  Mirror
	OB_MIRRORS                 []Mirror
)

func init() {
	if _, ok := architectureMap[runtime.GOARCH]; !ok {
		arch = runtime.GOARCH
	} else {
		arch = architectureMap[runtime.GOARCH]
	}

	if arch == AARCH64 {
		supoortLse = executeLocal("bash", "-c", "grep atomics /proc/cpuinfo").Code == 0
	} else {
		supoortLse = true
	}

	var version string
	ret := executeLocal("ldd", "--version")
	if ret.Stdout != "" {
		version = ret.Stdout
	} else {
		version = ret.Stderr
	}

	release = EL7
	pattern := regexp.MustCompile(`ldd\s+(\d+\.\d+)`)
	match := pattern.FindStringSubmatch(version)
	if match != nil && util.CmpVersionString(match[0], "2.28") >= 0 {
		release = EL8
	}

	OB_COMMUNITY_STABLE_MIRROR = OB_COMMUNITY_STABLE_BASE.GetMirror(arch, release)
	OB_DEVELOPMENT_KIT_MIRROR = OB_DEVELOPMENT_KIT_BASE.GetMirror(arch, release)
	OB_MIRRORS = []Mirror{OB_COMMUNITY_STABLE_MIRROR, OB_DEVELOPMENT_KIT_MIRROR}
}

func (m Mirror) getRepomdUrl() (string, error) {
	return url.JoinPath(m.url, REMOTE_REPOMD_FILE)
}

func (m Mirror) getRepoMD() (repo *repoMD, err error) {
	url, err := m.getRepomdUrl()
	if err != nil {
		return nil, err
	}

	req := resty.New().R()
	resq, err := req.Get(url)
	if err != nil {
		return nil, err
	}
	defer resq.RawResponse.Body.Close()

	xml.Unmarshal(resq.Body(), &repo)
	return
}

func (m Mirror) getLocalUrl(location Location) (string, error) {
	if location.BaseUrl != "" {
		return url.JoinPath(location.BaseUrl, location.Href)
	} else {
		return url.JoinPath(m.url, location.Href)
	}
}

func (m Mirror) getRepoPrimaryUrl() (string, error) {
	repo, err := m.getRepoMD()
	if err != nil {
		return "", err
	}
	for _, data := range repo.Data {
		if data.Type == PRIMARY_REPOMD_TYPE {
			return m.getLocalUrl(data.Location)
		}
	}
	return "", fmt.Errorf("primary repomd not found")
}

func (m Mirror) getRepoPrimary() (*primaryData, error) {
	url, err := m.getRepoPrimaryUrl()
	if err != nil {
		return nil, err
	}

	req := resty.New().R()
	resq, err := req.Get(url)
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(resq.Body())
	gzipReader, err := gzip.NewReader(buf)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()

	var packages primaryData
	err = xml.NewDecoder(gzipReader).Decode(&packages)
	return &packages, err
}

func (m Mirror) downloadPackage(packageInfo packageInfo, destDir string) (string, error) {
	if !filepath.IsAbs(destDir) {
		return "", fmt.Errorf("destination is not an absolute path: %v", destDir)
	}
	stat, err := os.Stat(destDir)
	if os.IsNotExist(err) {
		if err = os.MkdirAll(destDir, fs.FileMode(0755)); err != nil {
			return "", err
		}
	} else if !stat.IsDir() {
		return "", fmt.Errorf("destination is not a directory: %v", destDir)
	} else if err != nil {
		return "", err
	}

	url, err := m.getLocalUrl(packageInfo.Location)
	if err != nil {
		return "", err
	}

	req := resty.New().R()
	resq, err := req.Get(url)
	if err != nil {
		return "", err
	}
	defer resq.RawResponse.Body.Close()

	dest := filepath.Join(destDir, filepath.Base(packageInfo.Location.Href))
	if err = writeFileLocal(resq.Body(), dest, fs.FileMode(0644)); err != nil {
		return "", err
	}
	return dest, nil
}

// Download searches for the specified package entry in the mirror and downloads the first match to the destination directory.
// If no matching package is found, or an error occurs during the search, it returns an error.
func (m Mirror) Download(destDir string, entry PackageEntry) (string, error) {
	packages, err := m.Search(entry)
	if err != nil {
		return "", err
	}
	// If err is nil, then packages must not be nil
	return m.downloadPackage(packages[0], destDir)
}

// Search looks for packages that match the provided package entry within the mirror.
// If no matching packages are found or an error occurs during the search, it returns an error.
func (m Mirror) Search(entry PackageEntry) ([]packageInfo, error) {
	match, err := m.search(entry)
	if err != nil {
		return nil, err
	} else if len(match) == 0 {
		return nil, fmt.Errorf("no such package: %s-%s-%s", entry.Name, entry.Version, entry.Release)
	}
	return match, nil
}

func (m Mirror) search(entry PackageEntry) ([]packageInfo, error) {
	if entry.Name == "" {
		return nil, fmt.Errorf("package name is empty")
	}

	packages, err := m.getRepoPrimary()
	if err != nil {
		return nil, err
	}

	match := make([]packageInfo, 0)
	for _, pkg := range packages.Packages {
		if pkg.Name == entry.Name {
			if entry.Flags != "" && entry.Flags != pkg.Format.Provides[0].Flags {
				continue
			}
			if entry.Epoch != "" && entry.Epoch != pkg.Version.Epoch {
				continue
			}
			if entry.Version != "" && entry.Version != pkg.Version.Version {
				continue
			}
			if entry.Release != "" && entry.Release != pkg.Version.Release {
				continue
			}
			match = append(match, pkg)
		}
	}
	m.sortPackages(match)
	return match, nil
}

func (m Mirror) sortPackages(packages []packageInfo) {
	sort.Slice(packages, func(i, j int) bool {
		val := util.CmpVersionString(packages[i].Version.Epoch, packages[j].Version.Epoch)
		if val != 0 {
			return val > 0
		}
		val = util.CmpVersionString(packages[i].Version.Version, packages[j].Version.Version)
		if val != 0 {
			return val > 0
		}
		val = util.CmpVersionString(strings.Split(packages[i].Version.Release, ".")[0], strings.Split(packages[j].Version.Release, ".")[0])
		if val != 0 {
			return val > 0
		}
		if m.nonLse {
			return strings.Index(packages[i].Version.Release, NONLSE) > strings.Index(packages[j].Version.Release, NONLSE)
		} else if m.arch == AARCH64 {
			return strings.Index(packages[i].Version.Release, NONLSE) < strings.Index(packages[j].Version.Release, NONLSE)
		}
		return true
	})
}

// DownloadPackage searches for the specified package entry across all mirrors defined in OB_MIRRORS
// and downloads the first match to the destination directory.
// If no matching package is found in any mirror, or an error occurs, it returns an error.
func DownloadPackage(destDir string, entry PackageEntry) (string, error) {
	for _, mirror := range OB_MIRRORS {
		packages, err := mirror.search(entry)
		if err != nil {
			return "", err
		}

		if len(packages) > 0 {
			return mirror.downloadPackage(packages[0], destDir)
		}
	}
	return "", fmt.Errorf("no such package: %s-%s-%s", entry.Name, entry.Version, entry.Release)
}

// SearchPackage searches for packages that match the provided package entry across all mirrors defined in OB_MIRRORS.
// If no matching packages are found, or an error occurs during the search, it returns an error.
func SearchPackage(entry PackageEntry) ([]packageInfo, error) {
	for _, mirror := range OB_MIRRORS {
		packages, err := mirror.search(entry)
		if err != nil {
			return nil, err
		}

		if len(packages) > 0 {
			return packages, nil
		}
	}
	return nil, fmt.Errorf("no such package: %s-%s-%s", entry.Name, entry.Version, entry.Release)
}
