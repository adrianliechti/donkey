package analytics

import (
	"slices"
	"strings"
)

// Channels group where visits come from. A campaign's utm_medium wins over
// the referrer, because a paid or e-mailed link says what it is; a referrer
// alone is matched against well-known sites.
const (
	ChannelDirect   = "Direct"
	ChannelSearch   = "Search"
	ChannelSocial   = "Social"
	ChannelEmail    = "Email"
	ChannelPaid     = "Paid"
	ChannelAI       = "AI"
	ChannelReferral = "Referral"
)

var (
	paidMedia   = []string{"cpc", "ppc", "cpm", "cpv", "paid", "display", "banner", "retargeting", "affiliate"}
	emailMedia  = []string{"email", "e-mail", "newsletter", "mail"}
	socialMedia = []string{"social", "sm", "social-network", "social-media"}

	// Matched as the host itself or a parent domain of it, by name label:
	// "google" matches google.com, google.co.uk and www.google.de.
	searchSites = []string{"google", "bing.com", "duckduckgo.com", "yahoo", "baidu.com", "yandex", "ecosia.org",
		"search.brave.com", "startpage.com", "kagi.com", "qwant.com", "naver.com", "seznam.cz", "sogou.com"}
	socialSites = []string{"facebook.com", "fb.com", "m.facebook.com", "l.facebook.com", "instagram.com", "t.co",
		"twitter.com", "x.com", "linkedin.com", "lnkd.in", "reddit.com", "youtube.com", "tiktok.com", "pinterest.com",
		"threads.net", "bsky.app", "mastodon.social", "news.ycombinator.com", "tumblr.com", "vk.com", "weibo.com",
		"discord.com", "slack.com", "t.me", "whatsapp.com"}
	aiSites = []string{"chatgpt.com", "chat.openai.com", "perplexity.ai", "claude.ai", "gemini.google.com",
		"copilot.microsoft.com", "chat.mistral.ai", "you.com", "phind.com", "deepseek.com"}
	mailSites = []string{"mail.google.com", "outlook.live.com", "outlook.office.com", "mail.yahoo.com",
		"com.google.android.gm"}
)

// Channel classifies a visit's first page view.
func Channel(referrer, source, medium string) string {
	medium = strings.ToLower(medium)
	switch {
	case containsAny(medium, paidMedia):
		return ChannelPaid
	case containsAny(medium, emailMedia):
		return ChannelEmail
	case containsAny(medium, socialMedia):
		return ChannelSocial
	}
	host := referrer
	if host == "" {
		host = strings.ToLower(source)
	}
	switch {
	case host == "":
		return ChannelDirect
	case siteMatches(host, aiSites):
		return ChannelAI
	case siteMatches(host, mailSites):
		return ChannelEmail
	case siteMatches(host, searchSites):
		return ChannelSearch
	case siteMatches(host, socialSites):
		return ChannelSocial
	}
	return ChannelReferral
}

func containsAny(value string, list []string) bool {
	for _, item := range list {
		if value == item || strings.Contains(value, item) && len(item) > 3 {
			return true
		}
	}
	return false
}

// siteMatches compares by domain: the host or one of its parents is listed,
// or, for a bare brand ("google", "yahoo"), any of its labels is it.
func siteMatches(host string, sites []string) bool {
	for _, site := range sites {
		if !strings.Contains(site, ".") {
			if slices.Contains(strings.Split(host, "."), site) {
				return true
			}
			continue
		}
		if host == site || strings.HasSuffix(host, "."+site) {
			return true
		}
	}
	return false
}
