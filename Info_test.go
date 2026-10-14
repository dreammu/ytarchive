package main

import (
	"reflect"
	"testing"
)

func TestParseFormatPriority(t *testing.T) {
	got, err := ParseFormatPriority("720p:h264,1080p:h264,1440p:vp9,2160p:vp9")
	if err != nil {
		t.Fatalf("ParseFormatPriority returned an error: %v", err)
	}

	want := map[string]string{
		"720p": "h264", "1080p": "h264", "1440p": "vp9", "2160p": "vp9",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseFormatPriority() = %v, want %v", got, want)
	}
}

func TestParseFormatPriorityNormalizesAndUsesLastEntry(t *testing.T) {
	got, err := ParseFormatPriority(" 1080P : VP9, 1080p : H264 ")
	if err != nil {
		t.Fatalf("ParseFormatPriority returned an error: %v", err)
	}

	if got["1080p"] != "h264" {
		t.Fatalf("1080p priority = %q, want h264", got["1080p"])
	}
}

func TestParseFormatPriorityRejectsInvalidValues(t *testing.T) {
	tests := []string{
		"1080p:hevc",
		"foo:h264",
		"1080p",
		"1080p:h264:vp9",
		"",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseFormatPriority(value); err == nil {
				t.Fatalf("ParseFormatPriority(%q) did not return an error", value)
			}
		})
	}
}

func TestGetCodecPriorityOrderForQuality(t *testing.T) {
	di := NewDownloadInfo()
	di.FormatPriority = map[string]string{
		"1080p": "h264",
		"1440p": "vp9",
	}

	tests := []struct {
		quality string
		want    []string
	}{
		{"1080p", []string{"h264", "av1", "vp9"}},
		{"1080p60", []string{"h264", "av1", "vp9"}},
		{"1440p", []string{"vp9", "av1", "h264"}},
		{"1440p60", []string{"vp9", "av1", "h264"}},
		{"720p", []string{"av1", "vp9", "h264"}},
	}

	for _, test := range tests {
		t.Run(test.quality, func(t *testing.T) {
			got := di.GetCodecPriorityOrderForQuality(test.quality)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("codec order for %s = %v, want %v", test.quality, got, test.want)
			}
		})
	}
}

func TestGetCodecPriorityOrderForQualityPreservesRelativeOrder(t *testing.T) {
	di := NewDownloadInfo()
	di.VP9 = true
	di.FormatPriority["1080p"] = "h264"

	got := di.GetCodecPriorityOrderForQuality("1080p")
	want := []string{"h264", "vp9", "av1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codec order = %v, want %v", got, want)
	}
}

func TestGetCodecPriorityOrderForQualityPreservesFlagOrder(t *testing.T) {
	tests := []struct {
		name       string
		h264       bool
		vp9        bool
		av1        bool
		legacyWant []string
	}{
		{"none", false, false, false, []string{"av1", "vp9", "h264"}},
		{"h264", true, false, false, []string{"h264", "av1", "vp9"}},
		{"vp9", false, true, false, []string{"vp9", "av1", "h264"}},
		{"av1", false, false, true, []string{"av1", "vp9", "h264"}},
		{"h264-vp9", true, true, false, []string{"vp9", "h264", "av1"}},
		{"h264-av1", true, false, true, []string{"av1", "h264", "vp9"}},
		{"vp9-av1", false, true, true, []string{"av1", "vp9", "h264"}},
		{"all", true, true, true, []string{"av1", "vp9", "h264"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			di := NewDownloadInfo()
			di.H264 = test.h264
			di.VP9 = test.vp9
			di.AV1 = test.av1

			legacy := di.GetCodecPriorityOrder()
			if !reflect.DeepEqual(legacy, test.legacyWant) {
				t.Fatalf("legacy codec order = %v, want %v", legacy, test.legacyWant)
			}

			got := di.GetCodecPriorityOrderForQuality("720p")
			if !reflect.DeepEqual(got, legacy) {
				t.Fatalf("codec order without matching rule = %v, want %v", got, legacy)
			}
		})
	}
}

func TestSelectDownloadFormatsFallsBackWithinQuality(t *testing.T) {
	tests := []struct {
		name      string
		preferred string
		available string
		wantItag  int
	}{
		{"missing h264 falls back to vp9", "h264", "vp9", VideoLabelItags["1080p"].VP9},
		{"missing vp9 falls back to h264", "vp9", "h264", VideoLabelItags["1080p"].H264},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			di := NewDownloadInfo()
			di.FormatPriority["1080p"] = test.preferred
			videoItag := VideoLabelItags["1080p"]
			availableItag := videoItag.H264
			if test.available == "vp9" {
				availableItag = videoItag.VP9
			}
			dlUrls := map[int]string{
				AudioItag:     "https://example.com/audio",
				availableItag: "https://example.com/video",
			}

			if !di.SelectDownloadFormats(dlUrls, []string{"1080p"}) {
				t.Fatal("SelectDownloadFormats returned false")
			}
			if di.Quality != test.wantItag {
				t.Fatalf("selected itag = %d, want %d", di.Quality, test.wantItag)
			}
		})
	}
}
