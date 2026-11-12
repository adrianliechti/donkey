package analytics

import (
	"bufio"
	"bytes"
	_ "embed"
	"strings"
	"sync"
)

// zone.tab is the IANA time zone database's zone-to-country table (public
// domain). A browser's time zone names the country it is set up for without
// a GeoIP database or an IP address: coarser than geolocation, and exactly as
// coarse as a country report needs.
//
//go:embed zone.tab
var zoneTab []byte

// legacyZones are the backward-compatible names browsers still report, which
// zone.tab lists under their current name only.
var legacyZones = map[string]string{
	"Asia/Calcutta": "IN", "Asia/Saigon": "VN", "Asia/Katmandu": "NP", "Asia/Rangoon": "MM",
	"Asia/Ulan_Bator": "MN", "Asia/Dacca": "BD", "Asia/Thimbu": "BT", "Asia/Macao": "MO",
	"Asia/Ujung_Pandang": "ID", "Asia/Chongqing": "CN", "Asia/Harbin": "CN", "Asia/Tel_Aviv": "IL",
	"Asia/Istanbul": "TR", "Europe/Kiev": "UA", "Europe/Belfast": "GB", "Europe/Nicosia": "CY",
	"Atlantic/Faeroe": "FO", "America/Godthab": "GL", "America/Buenos_Aires": "AR",
	"America/Indianapolis": "US", "America/Louisville": "US", "America/Montreal": "CA",
	"Pacific/Truk": "FM", "Pacific/Ponape": "FM", "Pacific/Samoa": "AS", "Australia/Canberra": "AU",
	"Australia/ACT": "AU", "Australia/NSW": "AU", "US/Eastern": "US", "US/Central": "US",
	"US/Mountain": "US", "US/Pacific": "US", "US/Alaska": "US", "US/Hawaii": "US", "US/Arizona": "US",
	"Canada/Eastern": "CA", "Canada/Pacific": "CA", "Canada/Central": "CA", "Canada/Mountain": "CA",
	"Brazil/East": "BR", "Mexico/General": "MX", "GB": "GB", "Japan": "JP", "Singapore": "SG",
	"Hongkong": "HK", "Israel": "IL", "Turkey": "TR", "Egypt": "EG", "Iceland": "IS", "Iran": "IR",
	"Poland": "PL", "Portugal": "PT", "NZ": "NZ", "ROK": "KR", "PRC": "CN", "ROC": "TW", "Eire": "IE",
}

var zones = sync.OnceValue(func() map[string]string {
	out := make(map[string]string, 420)
	scanner := bufio.NewScanner(bytes.NewReader(zoneTab))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 && len(fields[0]) == 2 {
			out[fields[2]] = fields[0]
		}
	}
	return out
})

// Country is the ISO 3166 code of the country a time zone belongs to, or ""
// for UTC and anything unknown.
func Country(zone string) string {
	if country, ok := zones()[zone]; ok {
		return country
	}
	return legacyZones[zone]
}
