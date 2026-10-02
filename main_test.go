package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pixels(colours ...[3]uint16) []byte {
	data := make([]byte, len(colours)*pixelSize)
	for i, colour := range colours {
		for channel, value := range colour {
			binary.LittleEndian.PutUint16(data[i*pixelSize+channel*2:], value)
		}
	}
	return data
}

func TestPaletteRow(t *testing.T) {
	for _, tt := range []struct {
		name          string
		input, output [][3]uint16
	}{
		{"partial frames", [][3]uint16{{0, 0, 0}, {30000, 30000, 30000}, {60000, 60000, 60000}},
			[][3]uint16{{10000, 10000, 10000}, {50000, 50000, 50000}}},
		{"short clip", [][3]uint16{{60000, 0, 0}, {0, 0, 60000}},
			[][3]uint16{{60000, 0, 0}, {60000, 0, 0}, {30000, 0, 30000}, {0, 0, 60000}, {0, 0, 60000}}},
		{"single frame", [][3]uint16{{65535, 1, 32768}},
			[][3]uint16{{65535, 1, 32768}, {65535, 1, 32768}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paletteRow(bytes.NewReader(pixels(tt.input...)), int64(len(tt.input)), len(tt.output))
			if err != nil || !bytes.Equal(got, pixels(tt.output...)) {
				t.Fatalf("row = %v, error = %v; want %v", got, err, pixels(tt.output...))
			}
		})
	}
	if _, err := paletteRow(bytes.NewReader([]byte{0}), 1, 5); err == nil {
		t.Fatal("truncated frame must fail")
	}
	if _, err := paletteRow(bytes.NewReader(nil), 0, 5); err == nil {
		t.Fatal("empty clip must fail")
	}
}

func TestParseArgs(t *testing.T) {
	for _, args := range [][]string{
		{"clip.mp4", "out.png", "--width", "12000", "--force"},
		{"--force", "--width=12000", "clip.mp4", "out.png"},
	} {
		got, err := parseArgs(args)
		want := options{source: "clip.mp4", destination: "out.png", width: 12000, force: true}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("parse %v = %+v, %v", args, got, err)
		}
	}
	got, err := parseArgs([]string{"--", "-clip.mp4"})
	if err != nil || got.destination != "-clip-palette.png" {
		t.Fatalf("default output or literal filename incorrect: %+v, %v", got, err)
	}
	for _, args := range [][]string{nil, {"clip", "--width"}, {"clip", "--width=nope"}, {"clip", "--unknown"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestPipeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg required")
	}
	root := t.TempDir()
	source, output := filepath.Join(root, "video with spaces.mkv"), filepath.Join(root, "palette.png")
	red, blue := []byte{255, 0, 0}, []byte{0, 0, 255}
	stripe := append(bytes.Repeat(red, 8), bytes.Repeat(blue, 8)...)
	colours := [][]byte{
		bytes.Repeat(stripe, 16), bytes.Repeat([]byte{0, 255, 0}, 256),
		bytes.Repeat([]byte{255, 255, 255}, 256), make([]byte, 768), bytes.Repeat(blue, 256),
	}
	var raw []byte
	for _, colour := range colours {
		raw = append(raw, bytes.Repeat(colour, 2)...)
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "rawvideo",
		"-pixel_format", "rgb24", "-video_size", "16x16", "-framerate", "2",
		"-i", "pipe:0", "-c:v", "ffv1", source)
	cmd.Stdin = bytes.NewReader(raw)
	if message, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, message)
	}
	ctx := context.Background()
	count, err := createPalette(ctx, source, output, 5, false, io.Discard)
	if err != nil || count != 10 {
		t.Fatalf("create palette: frame count %d, error %v", count, err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if data[24] != 16 || data[25] != 2 {
		t.Fatal("expected 16-bit RGB PNG")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != 5 || img.Bounds().Dy() != 2 {
		t.Fatalf("incorrect dimensions or invalid PNG: %v", err)
	}
	want := [][3]uint32{{32768, 0, 32768}, {0, 65535, 0}, {65535, 65535, 65535}, {0, 0, 0}, {0, 0, 65535}}
	for x, target := range want {
		r, g, b, _ := img.At(x, 0).RGBA()
		for channel, value := range []uint32{r, g, b} {
			delta := int64(value) - int64(target[channel])
			if delta < -700 || delta > 700 {
				t.Fatalf("column %d: RGB %v; want approximately %v", x, []uint32{r, g, b}, target)
			}
		}
		if img.At(x, 0) != img.At(x, 1) {
			t.Fatal("stripe must extend to full image height")
		}
	}
	if _, err = createPalette(ctx, source, output, 5, false, io.Discard); err == nil {
		t.Fatal("existing file was overwritten without --force")
	}
	if _, err = createPalette(ctx, source, output, 6, true, io.Discard); err == nil {
		t.Fatal("width that breaks 5:2 ratio accepted")
	}
	corrupt := filepath.Join(root, "corrupt.mp4")
	if err = os.WriteFile(corrupt, []byte("invalid video"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = createPalette(ctx, corrupt, output, 5, true, io.Discard); err == nil {
		t.Fatal("corrupt video accepted")
	}
	unchanged, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(unchanged, data) {
		t.Fatal("failed operations modified existing output")
	}
	// Match the original implementation byte-for-byte, including PNG encoding.
	if python, err := exec.LookPath("python3"); err == nil {
		pythonOutput := filepath.Join(root, "python.png")
		cmd = exec.Command(python, "../python/movie_palette.py", source, pythonOutput, "--width", "5")
		if message, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Python comparison: %v: %s", err, message)
		}
		pythonData, err := os.ReadFile(pythonOutput)
		if err != nil || !bytes.Equal(data, pythonData) {
			t.Fatal("Go and Python output differ")
		}
	}
	if _, err = createPalette(ctx, source, output, 10, true, io.Discard); err != nil {
		t.Fatalf("--force failed: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".movie-palette-") {
			t.Fatalf("temporary output leaked: %s", entry.Name())
		}
	}
}

func BenchmarkPaletteRow(b *testing.B) {
	// Two hours at 24 fps. Excludes decoding and PNG encoding to isolate Go's work.
	const frameCount = 2 * 60 * 60 * 24
	data := make([]byte, frameCount*pixelSize)
	for i := 0; i < len(data); i++ {
		data[i] = byte(i)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := paletteRow(bytes.NewReader(data), frameCount, 6000); err != nil {
			b.Fatal(err)
		}
	}
}
