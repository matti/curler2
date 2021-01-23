package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/matti/stack"
	"github.com/montanaflynn/stats"
	"github.com/mum4k/termdash"
	"github.com/mum4k/termdash/cell"
	"github.com/mum4k/termdash/container"
	"github.com/mum4k/termdash/linestyle"
	"github.com/mum4k/termdash/terminal/tcell"
	"github.com/mum4k/termdash/terminal/terminalapi"
	"github.com/mum4k/termdash/widgets/linechart"
	"github.com/paulbellamy/ratecounter"
	"github.com/phayes/permbits"
)

type aggregation struct {
	min  float64
	mean float64
	max  float64
	p99  float64
}

var aggregations = make(chan aggregation, 1000)
var reset = make(chan bool, 1)
var trim = make(chan bool, 1)

func main() {
	nanoPath, _ := exec.LookPath("nano")

	var max string
	var rate int

	flag.StringVar(&max, "max", "5.0s", "max")
	flag.IntVar(&rate, "rate", 3, "rate")
	flag.Parse()

	inputFilePath := flag.Arg(0)

	var maxTime time.Duration
	if m, err := time.ParseDuration(max); err != nil {
		panic(err)
	} else {
		maxTime = m
	}

	var inputFile *os.File
	if u, err := url.Parse(inputFilePath); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if t, err := ioutil.TempFile("", ""); err != nil {
			panic(err)
		} else {
			inputFilePath = t.Name()
		}
		cmd := "curl " + u.String()

		ioutil.WriteFile(inputFilePath, []byte(cmd), os.FileMode(permbits.UserRead))
		if i, err := os.Open(inputFilePath); err != nil {
			panic(err)
		} else {
			inputFile = i
		}
	} else if _, err := os.Stat(inputFilePath); os.IsNotExist(err) {
		inputFile, _ = os.Create(inputFilePath)
		if proc, err := os.StartProcess(nanoPath, []string{"nano", inputFile.Name()}, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		}); err != nil {
			panic(err)
		} else if _, err = proc.Wait(); err != nil {
			panic(err)
		}
	} else {
		inputFile, _ = os.Open(inputFilePath)
	}

	var buf []byte
	if bytes, err := ioutil.ReadFile(inputFile.Name()); err != nil {
		panic(err)
	} else {
		buf = bytes
	}

	curlOrig := strings.TrimSpace(string(buf))
	curlLines := strings.Split(curlOrig, "\n")
	var betterCurlLines []string
	for _, line := range curlLines {
		betterCurlLines = append(betterCurlLines, strings.TrimSuffix(line, "\\"))
	}
	curlArgs := []string{
		"-L", "--silent", "-o /dev/null",
		"-w '%{time_starttransfer}\\n'", "--max-time " + fmt.Sprintf("%f", maxTime.Seconds()),
	}
	curlCmd := strings.Join(append(betterCurlLines, curlArgs...), " \\\n")

	outputFile, _ := ioutil.TempFile("", "")
	ioutil.WriteFile(outputFile.Name(), []byte(curlCmd), 0)

	bits := permbits.UserRead
	bits.SetUserExecute(true)
	if err := os.Chmod(outputFile.Name(), os.FileMode(bits)); err != nil {
		panic(err)
	}

	values := make(chan float64, rate)
	var data stats.Float64Data

	go func() {
		for {
			v := <-values
			data = append(data, v)
			if len(data) > rate {
				data = data[rate:]
			}
		}
	}()

	go func() {
		for {
			time.Sleep(250 * time.Millisecond)
			var min = 0.0
			var mean = 0.0
			var max = 0.0
			var p99 = 0.0

			if s, err := stats.Min(data); err == nil {
				min = s
			}
			if s, err := stats.Mean(data); err == nil {
				mean = s
			}
			if s, err := stats.Max(data); err == nil {
				max = s
			}
			if s, err := stats.Percentile(data, 99); err == nil {
				p99 = s
			}

			aggregations <- aggregation{
				min:  min,
				mean: mean,
				max:  max,
				p99:  p99,
			}
		}
	}()

	go func() {
		counter := ratecounter.NewRateCounter(1 * time.Second)
		inflight := stack.NewStack()
		for {
			for i := 0; i < rate; i++ {
				for {
					if counter.Rate() >= int64(rate) {
						time.Sleep(100 * time.Millisecond)
					} else {
						break
					}
				}
				for {
					if inflight.Size() == rate {
						time.Sleep(100 * time.Millisecond)
					} else {
						break
					}
				}

				counter.Incr(1)
				inflight.Push(1)
				go func() {
					values <- run(outputFile.Name())
					inflight.Pop()
				}()

			}
		}
	}()

	t, err := tcell.New()
	if err != nil {
		panic(err)
	}
	defer t.Close()

	ctx, cancel := context.WithCancel(context.Background())
	lc, err := linechart.New(
		linechart.AxesCellOpts(cell.FgColor(cell.ColorRed)),
		linechart.YLabelCellOpts(cell.FgColor(cell.ColorGreen)),
		linechart.XLabelCellOpts(cell.FgColor(cell.ColorCyan)),
	)
	if err != nil {
		panic(err)
	}
	go playLineChart(ctx, lc)
	c, err := container.New(
		t,
		container.Border(linestyle.Light),
		container.BorderTitle("PRESS Q TO QUIT"),
		container.PlaceWidget(lc),
	)
	if err != nil {
		panic(err)
	}

	quitter := func(k *terminalapi.Keyboard) {
		switch strings.ToLower(k.Key.String()) {
		case "q":
			cancel()
		case "r":
			reset <- true
		case "t":
			trim <- true
		}
	}

	if err := termdash.Run(ctx, t, c, termdash.KeyboardSubscriber(quitter)); err != nil {
		panic(err)
	}
}

func run(path string) (time float64) {
	ttfb := -1.0

	var stdout bytes.Buffer
	cmd := exec.Command("/usr/bin/env", "sh", "-c", path)
	cmd.Stdout = &stdout
	cmd.Run()
	if cmd.ProcessState.ExitCode() != 0 {
		return ttfb
	}

	if t, err := strconv.ParseFloat(strings.TrimSuffix(stdout.String(), "\n"), 64); err != nil {
		return ttfb
	} else {
		ttfb = t
	}

	return ttfb
}

func trimSlice(slice []float64, amount int) []float64 {
	var trimmed []float64

	if len(slice) >= amount {
		trimmed = slice[amount:]
	} else {
		trimmed = slice
	}

	return trimmed
}

func playLineChart(ctx context.Context, lc *linechart.LineChart) {
	var mins []float64
	var means []float64
	var maxes []float64
	var p99s []float64

	for {
		select {
		case <-trim:
			mins = trimSlice(mins, 100)
			means = trimSlice(means, 100)
			maxes = trimSlice(maxes, 100)
			p99s = trimSlice(p99s, 100)
		case <-reset:
			mins = []float64{}
			means = []float64{}
			maxes = []float64{}
			p99s = []float64{}
		case a := <-aggregations:
			mins = append(mins, a.min)
			means = append(means, a.mean)
			maxes = append(maxes, a.max)
			p99s = append(p99s, a.p99)

			if err := lc.Series("max", maxes,
				linechart.SeriesCellOpts(cell.FgColor(cell.ColorRed)),
			); err != nil {
				panic(err)
			}
			if err := lc.Series("p99", p99s,
				linechart.SeriesCellOpts(cell.FgColor(cell.ColorFuchsia)),
			); err != nil {
				panic(err)
			}

			if err := lc.Series("mean", means,
				linechart.SeriesCellOpts(cell.FgColor(cell.ColorYellow)),
			); err != nil {
				panic(err)
			}
			if err := lc.Series("min", mins,
				linechart.SeriesCellOpts(cell.FgColor(cell.ColorLime)),
			); err != nil {
				panic(err)
			}

		case <-ctx.Done():
			return
		}
	}
}
