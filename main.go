// movie-palette turns every video frame into a stripe in a 5:2 PNG using FFmpeg.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const pixelSize = 6 // Little-endian RGB, 16 bits per channel.

// paletteRow area-averages the timeline using exact integer overlap weights.
// Each source frame spans width units; each output column spans frameCount.
func paletteRow(frames io.Reader, frameCount int64, width int) ([]byte, error) {
	if frameCount <= 0 || frameCount > math.MaxInt64/65536 {
		return nil, errors.New("invalid frame count")
	}
	if width <= 0 || width > int(^uint(0)>>1)/pixelSize {
		return nil, errors.New("invalid width")
	}
	reader := bufio.NewReaderSize(frames, 64*1024)
	var pixel [pixelSize]byte
	var colour [3]int64
	row := make([]byte, width*pixelSize)
	var remaining int64
	for x := 0; x < width; x++ {
		needed := frameCount
		var sums [3]int64
		for needed > 0 {
			if remaining == 0 {
				if _, err := io.ReadFull(reader, pixel[:]); err != nil {
					return nil, fmt.Errorf("read frame colour: %w", err)
				}
				for channel := range colour {
					colour[channel] = int64(binary.LittleEndian.Uint16(pixel[channel*2:]))
				}
				remaining = int64(width)
			}
			weight := min(remaining, needed)
			for channel := range sums {
				sums[channel] += colour[channel] * weight
			}
			remaining -= weight
			needed -= weight
		}
		for channel, sum := range sums {
			value := uint16((sum + frameCount/2) / frameCount)
			binary.LittleEndian.PutUint16(row[x*pixelSize+channel*2:], value)
		}
	}
	return row, nil
}

func runFFmpeg(ctx context.Context, executable string, args []string, stdin io.Reader, stdout, progress io.Writer) error {
	base := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-stats", "-stats_period", "1"}
	cmd := exec.CommandContext(ctx, executable, append(base, args...)...)
	cmd.Stdin, cmd.Stdout = stdin, stdout
	// Stream live frame/time/speed updates and errors instead of buffering them.
	cmd.Stderr = progress
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("FFmpeg: %w; see its output above", err)
	}
	return nil
}

func resolvePath(name string) (string, error) {
	if name == "~" || strings.HasPrefix(name, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		name = filepath.Join(home, strings.TrimPrefix(name, "~"))
	}
	path, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func createPalette(ctx context.Context, source, destination string, width int, force bool, progress io.Writer) (int64, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, errors.New("FFmpeg is required; install it and make sure it is on PATH")
	}
	source, err = resolvePath(source)
	if err != nil {
		return 0, fmt.Errorf("input: %w", err)
	}
	destination, err = resolvePath(destination)
	if err != nil {
		return 0, fmt.Errorf("output: %w", err)
	}
	inputInfo, err := os.Stat(source)
	if err != nil || !inputInfo.Mode().IsRegular() {
		return 0, fmt.Errorf("video file not found or not a regular file: %s", source)
	}
	outputInfo, statErr := os.Stat(destination)
	if source == destination || (statErr == nil && os.SameFile(inputInfo, outputInfo)) {
		return 0, errors.New("the output must be different from the input video")
	}
	if strings.ToLower(filepath.Ext(destination)) != ".png" {
		return 0, errors.New("use a .png output filename for a lossless image")
	}
	if width < 5 || width%5 != 0 || width > int(^uint(0)>>1)/pixelSize {
		return 0, errors.New("width must be a positive multiple of 5 (e.g. 6000 or 12000)")
	}
	if statErr == nil {
		if !force {
			return 0, fmt.Errorf("output already exists: %s; use --force to replace it", destination)
		}
		if !outputInfo.Mode().IsRegular() {
			return 0, errors.New("output is not a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return 0, statErr
	}
	started := time.Now()
	height := width / 5 * 2

	// The intermediate file uses only six bytes per frame, regardless of resolution.
	frames, err := os.CreateTemp("", "movie-palette-frames-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(frames.Name())
	defer frames.Close()
	fmt.Fprintln(progress, "[1/3] Averaging every frame with FFmpeg…")
	err = runFFmpeg(ctx, ffmpeg, []string{
		"-i", source, "-map", "0:V:0", "-an", "-sn", "-dn",
		"-vf", "format=rgb48le,scale=1:1:flags=area+accurate_rnd,setsar=1",
		"-fps_mode", "passthrough", "-c:v", "rawvideo",
		"-pix_fmt", "rgb48le", "-f", "rawvideo", "pipe:1",
	}, nil, frames, progress)
	if err != nil {
		return 0, err
	}
	info, err := frames.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 || info.Size()%pixelSize != 0 {
		return 0, errors.New("FFmpeg did not produce complete video frame colours")
	}
	frameCount := info.Size() / pixelSize
	fmt.Fprintf(progress, "[2/3] Combining %d frames into %d stripes…\n", frameCount, width)
	if _, err = frames.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	row, err := paletteRow(frames, frameCount, width)
	if err != nil {
		return 0, err
	}

	// Encode beside the destination and publish only after successful completion.
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".movie-palette-*.png")
	if err != nil {
		return 0, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err = temporary.Close(); err != nil {
		return 0, err
	}
	fmt.Fprintf(progress, "[3/3] Encoding %d × %d PNG…\n", width, height)
	err = runFFmpeg(ctx, ffmpeg, []string{
		"-y", "-f", "rawvideo", "-pixel_format", "rgb48le",
		"-video_size", fmt.Sprintf("%dx1", width), "-i", "pipe:0",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=neighbor,setsar=1", width, height),
		"-frames:v", "1", "-c:v", "png", "-pix_fmt", "rgb48be",
		"-compression_level", "9", "-update", "1", temporaryName,
	}, bytes.NewReader(row), nil, progress)
	if err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if force {
		err = os.Rename(temporaryName, destination)
	} else {
		// Link refuses to overwrite a file created while processing the video.
		err = os.Link(temporaryName, destination)
	}
	if err != nil {
		return 0, fmt.Errorf("save output: %w", err)
	}
	fmt.Fprintf(progress, "Saved %s (%d × %d, 16-bit RGB PNG) in %.1fs\n",
		destination, width, height, time.Since(started).Seconds())
	return frameCount, nil
}

type options struct {
	source, destination string
	width               int
	force, help         bool
}

// Accept options before or after filenames, matching the Python CLI.
func parseArgs(args []string) (options, error) {
	result := options{width: 6000}
	var positional []string
	literal := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case literal:
			positional = append(positional, arg)
		case arg == "--":
			literal = true
		case arg == "--help" || arg == "-h":
			result.help = true
			return result, nil
		case arg == "--force":
			result.force = true
		case arg == "--width" || strings.HasPrefix(arg, "--width="):
			value := strings.TrimPrefix(arg, "--width=")
			if arg == "--width" {
				i++
				if i == len(args) {
					return result, errors.New("--width requires a value")
				}
				value = args[i]
			}
			width, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("invalid width: %s", value)
			}
			result.width = width
		case strings.HasPrefix(arg, "-"):
			return result, fmt.Errorf("unknown option: %s", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) < 1 || len(positional) > 2 {
		return result, errors.New("provide an input video and optionally an output PNG")
	}
	result.source = positional[0]
	name := filepath.Base(result.source)
	if extension := filepath.Ext(name); extension != name && extension != "." {
		name = strings.TrimSuffix(name, extension)
	}
	result.destination = filepath.Join(filepath.Dir(result.source), name+"-palette.png")
	if len(positional) == 2 {
		result.destination = positional[1]
	}
	return result, nil
}

func run() int {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		fmt.Fprintln(os.Stderr, "Use --help for usage.")
		return 2
	}
	if opts.help {
		fmt.Println(`Usage: movie-palette VIDEO [OUTPUT.png] [--width 6000] [--force]

Turn every video frame into a stripe in a 5:2, 16-bit RGB PNG.
Requires FFmpeg 5+ on PATH. Default output: <video-stem>-palette.png.

  --width N    Output width, a positive multiple of 5 (default: 6000 × 2400)
  --force      Replace an existing output PNG
  -h, --help   Show this help

Options can appear before or after filenames. Use -- before names starting with -.`)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	_, err = createPalette(ctx, opts.source, opts.destination, opts.width, opts.force, os.Stderr)
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "Cancelled.")
		return 130
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run())
}
