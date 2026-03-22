package builder

import (
	"net/url"
	"strings"

	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/model"
)

func ParseURL(link string) (model.Info, error) {
	parsed, err := parseURL(link)
	if err != nil {
		return model.Info{}, err
	}

	info := model.Info{}

	if strings.HasSuffix(parsed.Host, "youtube.com") {
		kind, id, err := parseYoutubeURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderYoutube
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	if strings.HasSuffix(parsed.Host, "vimeo.com") {
		kind, id, err := parseVimeoURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderVimeo
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	if strings.HasSuffix(parsed.Host, "soundcloud.com") {
		kind, id, err := parseSoundcloudURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderSoundcloud
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	if strings.HasSuffix(parsed.Host, "twitch.tv") {
		kind, id, err := parseTwitchURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderTwitch
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	if strings.HasSuffix(parsed.Host, "odysee.com") {
		kind, id, err := parseOdyseeURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderOdysee
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	if strings.HasSuffix(parsed.Host, "rumble.com") {
		kind, id, err := parseRumbleURL(parsed)
		if err != nil {
			return model.Info{}, err
		}

		info.Provider = model.ProviderRumble
		info.LinkType = kind
		info.ItemID = id

		return info, nil
	}

	return model.Info{}, errors.New("unsupported URL host")
}

func parseURL(link string) (*url.URL, error) {
	if !strings.HasPrefix(link, "http") {
		link = "https://" + link
	}

	parsed, err := url.Parse(link)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse url: %s", link)
	}

	return parsed, nil
}

func parseYoutubeURL(parsed *url.URL) (model.Type, string, error) {
	path := parsed.EscapedPath()

	// https://www.youtube.com/playlist?list=PLCB9F975ECF01953C
	// https://www.youtube.com/watch?v=rbCbho7aLYw&list=PLMpEfaKcGjpWEgNtdnsvLX6LzQL0UC0EM
	if strings.HasPrefix(path, "/playlist") || strings.HasPrefix(path, "/watch") {
		kind := model.TypePlaylist

		id := parsed.Query().Get("list")
		if id != "" {
			return kind, id, nil
		}

		return "", "", errors.New("invalid playlist link")
	}

	// - https://www.youtube.com/channel/UC5XPnUk8Vvv_pWslhwom6Og
	// - https://www.youtube.com/channel/UCrlakW-ewUT8sOod6Wmzyow/videos
	if strings.HasPrefix(path, "/channel") {
		kind := model.TypeChannel
		parts := strings.Split(parsed.EscapedPath(), "/")
		if len(parts) <= 2 {
			return "", "", errors.New("invalid youtube channel link")
		}

		id := parts[2]
		if id == "" {
			return "", "", errors.New("invalid id")
		}

		return kind, id, nil
	}

	// - https://www.youtube.com/user/fxigr1
	if strings.HasPrefix(path, "/user") {
		kind := model.TypeUser

		parts := strings.Split(parsed.EscapedPath(), "/")
		if len(parts) <= 2 {
			return "", "", errors.New("invalid user link")
		}

		id := parts[2]
		if id == "" {
			return "", "", errors.New("invalid id")
		}

		return kind, id, nil
	}

	// - https://www.youtube.com/@username
	// - https://www.youtube.com/@username/videos
	if strings.HasPrefix(path, "/@") {
		kind := model.TypeHandle

		parts := strings.Split(parsed.EscapedPath(), "/")
		if len(parts) <= 1 {
			return "", "", errors.New("invalid handle link")
		}

		handle := parts[1]
		if handle == "" || !strings.HasPrefix(handle, "@") {
			return "", "", errors.New("invalid handle format")
		}

		// Remove the @ prefix for storage
		id := strings.TrimPrefix(handle, "@")
		if id == "" {
			return "", "", errors.New("empty handle")
		}

		return kind, id, nil
	}

	return "", "", errors.New("unsupported link format")
}

func parseVimeoURL(parsed *url.URL) (model.Type, string, error) {
	parts := strings.Split(parsed.EscapedPath(), "/")
	if len(parts) <= 1 {
		return "", "", errors.New("invalid vimeo link path")
	}

	var kind model.Type
	switch parts[1] {
	case "groups":
		kind = model.TypeGroup
	case "channels":
		kind = model.TypeChannel
	default:
		kind = model.TypeUser
	}

	if kind == model.TypeGroup || kind == model.TypeChannel {
		if len(parts) <= 2 {
			return "", "", errors.New("invalid channel link")
		}

		id := parts[2]
		if id == "" {
			return "", "", errors.New("invalid id")
		}

		return kind, id, nil
	}

	if kind == model.TypeUser {
		id := parts[1]
		if id == "" {
			return "", "", errors.New("invalid id")
		}

		return kind, id, nil
	}

	return "", "", errors.New("unsupported link format")
}

func parseSoundcloudURL(parsed *url.URL) (model.Type, string, error) {
	parts := strings.Split(parsed.EscapedPath(), "/")
	if len(parts) <= 3 {
		return "", "", errors.New("invald soundcloud link path")
	}

	var kind model.Type

	// - https://soundcloud.com/user/sets/example-set
	switch parts[2] {
	case "sets":
		kind = model.TypePlaylist
	default:
		return "", "", errors.New("invalid soundcloud url, missing sets")
	}

	id := parts[3]

	return kind, id, nil
}

func parseTwitchURL(parsed *url.URL) (model.Type, string, error) {
	// - https://www.twitch.tv/samueletienne
	path := parsed.EscapedPath()
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", "", errors.Errorf("invald twitch user path: %s", path)
	}

	kind := model.TypeUser

	id := parts[1]
	if id == "" {
		return "", "", errors.New("invalid id")
	}

	return kind, id, nil
}

func parseOdyseeURL(parsed *url.URL) (model.Type, string, error) {
	// - https://odysee.com/@theduran:e
	// - https://odysee.com/@theduran:e/video-name
	path := parsed.EscapedPath()
	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		return "", "", errors.New("invalid odysee path")
	}

	handle := parts[1]
	if handle == "" || !strings.HasPrefix(handle, "@") {
		return "", "", errors.New("invalid odysee handle format")
	}

	// Extract the channel ID (everything after @ including the :xxx suffix)
	id := strings.TrimPrefix(handle, "@")
	if id == "" {
		return "", "", errors.New("empty odysee channel id")
	}

	kind := model.TypeChannel

	return kind, id, nil
}

func parseRumbleURL(parsed *url.URL) (model.Type, string, error) {
	// - https://rumble.com/c/creator-name
	// - https://rumble.com/c/c-123456
	// - https://rumble.com/vXXXXXX-video-title.html
	path := parsed.EscapedPath()
	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		return "", "", errors.New("invalid rumble path")
	}

	// Channel format: /c/... or /c/c-NUMBER
	if parts[1] == "c" && len(parts) > 2 && parts[2] != "" {
		channelName := parts[2]

		// Convert channel name to channel ID if needed
		// yt-dlp handles both formats, but we'll use the channel identifier as-is
		kind := model.TypeChannel
		return kind, channelName, nil
	}

	// Individual video format: /vXXXXXX or /vXXXXXX-title.html
	if strings.HasPrefix(parts[1], "v") && len(parts[1]) > 1 {
		videoID := parts[1]
		// Remove .html extension if present
		videoID = strings.TrimSuffix(videoID, ".html")

		kind := model.TypePlaylist
		return kind, videoID, nil
	}

	return "", "", errors.New("unsupported rumble link format")
}
