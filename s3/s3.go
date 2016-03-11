// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package s3

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/template"
	"github.com/blang/semver"
	"github.com/goamz/goamz/aws"
	"github.com/goamz/goamz/s3"
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/keybase/release/update"
	"github.com/keybase/release/version"
)

type Section struct {
	Header   string
	Releases []Release
}

type Release struct {
	Name       string
	Key        s3.Key
	URL        string
	Version    string
	DateString string
	Date       time.Time
	Commit     string
}

type ByRelease []Release

func (s ByRelease) Len() int {
	return len(s)
}

func (s ByRelease) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByRelease) Less(i, j int) bool {
	// Reverse date order
	return s[j].Date.Before(s[i].Date)
}

type Client struct {
	s3 *s3.S3
}

func NewClient() (*Client, error) {
	auth, err := aws.EnvAuth()
	if err != nil {
		return nil, err
	}
	s3 := s3.New(auth, aws.USEast)
	return &Client{s3: s3}, nil
}

func convertEastern(t time.Time) time.Time {
	locationNewYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		log.Printf("Couldn't load location: %s", err)
	}
	return t.In(locationNewYork)
}

func loadReleases(keys []s3.Key, bucketName string, prefix string, suffix string, truncate int) []Release {
	var releases []Release
	for _, k := range keys {
		if strings.HasSuffix(k.Key, suffix) {
			key := k.Key
			name := key[len(prefix):]
			urlString := fmt.Sprintf("https://s3.amazonaws.com/%s/%s%s", bucketName, prefix, url.QueryEscape(name))
			version, date, commit, err := version.Parse(name)
			if err != nil {
				log.Printf("Couldn't get version from name: %s\n", name)
			}
			date = convertEastern(date)
			releases = append(releases,
				Release{
					Name:       name,
					Key:        k,
					URL:        urlString,
					Version:    version,
					Date:       date,
					DateString: date.Format("Mon Jan _2 15:04:05 MST 2006"),
					Commit:     commit,
				})
		}
	}
	// TODO: Should also sanity check that version sort is same as time sort
	// otherwise something got messed up
	sort.Sort(ByRelease(releases))
	if truncate > 0 && len(releases) > truncate {
		releases = releases[0:truncate]
	}
	return releases
}

func WriteHTML(path string, bucketName string, prefixes string, suffix string) error {
	client, err := NewClient()
	if err != nil {
		return err
	}
	bucket := client.s3.Bucket(bucketName)
	if bucket == nil {
		return fmt.Errorf("Bucket %s not found", bucketName)
	}

	var sections []Section
	for _, prefix := range strings.Split(prefixes, ",") {
		resp, err := bucket.List(prefix, "", "", 0)
		if err != nil {
			return err
		}

		releases := loadReleases(resp.Contents, bucketName, prefix, suffix, 20)
		if len(releases) > 0 {
			log.Printf("Found %d release(s) at %s\n", len(releases), prefix)
			for _, release := range releases {
				log.Printf(" %s %s %s\n", release.Name, release.Version, release.DateString)
			}
		}
		sections = append(sections, Section{
			Header:   prefix,
			Releases: releases,
		})
	}

	return WriteHTMLForLinks(path, bucketName, sections)
}

var htmlTemplate = `
<!doctype html>
<html lang="en">
<head>
  <title>{{ .Title }}</title>
	<style>
  body { font-family: monospace; }
  </style>
</head>
<body>
	{{ range $index, $sec := .Sections }}
		<h3>{{ $sec.Header }}</h3>
		<ul>
		{{ range $index2, $rel := $sec.Releases }}
		<li><a href="{{ $rel.URL }}">{{ $rel.Name }}</a> <strong>{{ $rel.Version }}</strong> <em>{{ $rel.Date }}</em> <a href="https://github.com/keybase/client/commit/{{ $rel.Commit }}"">{{ $rel.Commit }}</a></li>
		{{ end }}
		</ul>
	{{ end }}
</body>
</html>
`

func WriteHTMLForLinks(path string, title string, sections []Section) error {
	vars := map[string]interface{}{
		"Title":    title,
		"Sections": sections,
	}

	t, err := template.New("t").Parse(htmlTemplate)
	if err != nil {
		return err
	}

	if path != "" {
		var data bytes.Buffer
		err = t.Execute(&data, vars)
		if err != nil {
			return err
		}
		err := makeParentDirs(path)
		if err != nil {
			return err
		}
		return ioutil.WriteFile(path, data.Bytes(), 0644)
	}
	return nil
}

type Platform struct {
	Name          string
	Prefix        string
	PrefixSupport string
	Suffix        string
	LatestName    string
}

func CopyLatest(bucketName string) error {
	client, err := NewClient()
	if err != nil {
		return err
	}
	return client.CopyLatest(bucketName)
}

func Platforms() []Platform {
	return []Platform{
		Platform{Name: "darwin", Prefix: "darwin/", PrefixSupport: "darwin-support/", LatestName: "Keybase.dmg"},
		Platform{Name: "deb", Prefix: "linux_binaries/deb/", Suffix: "_amd64.deb", LatestName: "keybase_amd64.deb"},
		Platform{Name: "rpm", Prefix: "linux_binaries/rpm/", Suffix: ".x86_64.rpm", LatestName: "keybase_amd64.rpm"},
		Platform{Name: "windows", Prefix: "windows/", Suffix: ".386.exe", LatestName: "keybase_setup_386.exe"},
	}
}

func FindPlatform(name string) *Platform {
	platforms := Platforms()
	for _, p := range platforms {
		if p.Name == name {
			return &p
		}
	}
	return nil
}

func (p *Platform) FindRelease(bucket s3.Bucket, f func(r Release) bool) (*Release, error) {
	resp, err := bucket.List(p.Prefix, "", "", 0)
	if err != nil {
		return nil, err
	}
	releases := loadReleases(resp.Contents, bucket.Name, p.Prefix, p.Suffix, 0)
	for _, release := range releases {
		k := release.Key
		if !strings.HasSuffix(k.Key, p.Suffix) {
			continue
		}
		if f(release) {
			return &release, nil
		}
	}
	return nil, nil
}

func (c *Client) CopyLatest(bucketName string) error {
	bucket := c.s3.Bucket(bucketName)

	platforms := Platforms()

	for _, platform := range platforms {
		release, err := platform.FindRelease(*bucket, func(r Release) bool { return true })
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}
		k := release.Key
		url := urlString(k, bucketName, platform.Prefix)
		// Instead of linking, we're making copies. S3 linking has some issues.
		// headers := map[string][]string{
		// 	"x-amz-website-redirect-location": []string{url},
		// }
		//err = bucket.PutHeader(name, []byte{}, headers, s3.PublicRead)
		log.Printf("Copying %s from %s (latest)\n", platform.LatestName, k.Key)
		_, err = bucket.PutCopy(platform.LatestName, s3.PublicRead, s3.CopyOptions{}, url)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) CurrentUpdate(bucketName string, platform string, env string) (currentUpdate *keybase1.Update, err error) {
	bucket := c.s3.Bucket(bucketName)

	data, err := bucket.Get(fmt.Sprintf("update-%s-%s.json", platform, env))
	if err != nil {
		return
	}
	currentUpdate, err = update.DecodeJSON(data)
	return
}

func PromoteRelease(bucketName string, delay time.Duration, hourEastern int, channel string, platform string, env string) (*Release, error) {
	client, err := NewClient()
	if err != nil {
		return nil, err
	}
	return client.PromoteRelease(bucketName, delay, hourEastern, channel, platform, env)
}

func updateJSONName(channel string, platformName string, env string) string {
	if channel == "" {
		return fmt.Sprintf("update-%s-%s.json", platformName, env)
	}
	return fmt.Sprintf("update-%s-%s-%s.json", platformName, env, channel)
}

func (c *Client) PromoteRelease(bucketName string, delay time.Duration, hourEastern int, channel string, platformName string, env string) (*Release, error) {
	if channel == "" {
		log.Printf("Finding release to promote (%s delay)", delay)
	} else {
		log.Printf("Finding release to promote for %s channel (%s delay)", channel, delay)
	}
	bucket := c.s3.Bucket(bucketName)

	platform := FindPlatform(platformName)
	if platform == nil {
		return nil, fmt.Errorf("Unsupported platform")
	}
	release, err := platform.FindRelease(*bucket, func(r Release) bool {
		hour, min, _ := r.Date.Clock()
		return (time.Since(r.Date) >= delay && hour < hourEastern && min < 15)
	})
	if err != nil {
		return nil, err
	}

	if release == nil {
		return nil, nil
	}
	log.Printf("Found release %s (%s), %s", release.Name, time.Since(release.Date), release.Version)

	currentUpdate, err := c.CurrentUpdate(bucketName, platformName, env)
	if err != nil {
		return nil, err
	}
	if currentUpdate != nil {
		log.Printf("Found update: %s", currentUpdate.Version)
		currentVer, err := semver.Make(currentUpdate.Version)
		if err != nil {
			return nil, err
		}
		releaseVer, err := semver.Make(release.Version)
		if err != nil {
			return nil, err
		}

		if releaseVer.Equals(currentVer) {
			log.Printf("Release unchanged")
			return nil, nil
		} else if releaseVer.LT(currentVer) {
			log.Printf("Release older than current update")
			return nil, nil
		}
	}

	jsonName := updateJSONName(channel, platformName, env)
	jsonURL := fmt.Sprintf("https://s3.amazonaws.com/%supdate-%s-%s-%s.json", platform.PrefixSupport, platformName, env, release.Version)
	log.Printf("Promoting %s", jsonURL)
	_, err = bucket.PutCopy(jsonName, s3.PublicRead, s3.CopyOptions{}, jsonURL)
	if err != nil {
		return nil, err
	}
	return release, nil
}
