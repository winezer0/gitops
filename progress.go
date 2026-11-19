package gitops

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	progressPercent         = regexp.MustCompile(`([0-9]{1,3})%`)
	receivingObjectsPattern = regexp.MustCompile(`(?i)receiving objects:\s*([0-9]{1,3})%\s*\(\s*([\d,]+)\s*/\s*([\d,]+)\s*\)`)
	receivedBytesPattern    = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(bytes?|b|kib|mib|gib|tib|kb|mb|gb|tb)\b`)
)

const progressLineLimit = 4096

type progressWriter struct {
	mu        sync.Mutex
	line      []byte
	engine    string
	operation string
	callback  func(ProgressEvent)
	observer  func(OperationEvent)
}

func (writer *progressWriter) Write(output []byte) (int, error) {
	lines := make([]string, 0, 2)
	writer.mu.Lock()
	for _, value := range output {
		if value == '\r' || value == '\n' {
			if len(writer.line) > 0 {
				lines = append(lines, string(writer.line))
				writer.line = writer.line[:0]
			}
			continue
		}
		if len(writer.line) == progressLineLimit {
			copy(writer.line, writer.line[1:])
			writer.line = writer.line[:progressLineLimit-1]
		}
		writer.line = append(writer.line, value)
	}
	writer.mu.Unlock()
	for _, line := range lines {
		writer.report(line)
	}
	return len(output), nil
}

func (writer *progressWriter) Flush() {
	writer.mu.Lock()
	line := string(writer.line)
	writer.line = writer.line[:0]
	writer.mu.Unlock()
	if line != "" {
		writer.report(line)
	}
}

func (writer *progressWriter) report(line string) {
	phase := progressPhase(line)
	event := ProgressEvent{Engine: writer.engine, Phase: phase}
	if matches := progressPercent.FindStringSubmatch(line); len(matches) == 2 {
		event.Percent, _ = strconv.Atoi(matches[1])
	}
	if matches := receivingObjectsPattern.FindStringSubmatch(line); len(matches) == 4 {
		event.Percent, _ = strconv.Atoi(matches[1])
		event.ReceivedObjects, _ = parseProgressCount(matches[2])
		event.TotalObjects, _ = parseProgressCount(matches[3])
		if size := receivedBytesPattern.FindStringSubmatch(line); len(size) == 3 {
			event.BytesReceived = parseProgressBytes(size[1], size[2])
		}
	}
	if event.Percent == 0 && event.ReceivedObjects == 0 && event.TotalObjects == 0 {
		return
	}
	if writer.callback != nil {
		writer.callback(event)
	}
	emitProgress(writer.observer, OperationEvent{
		Kind: EventProgress, Operation: writer.operation, Engine: writer.engine, Phase: event.Phase,
		Percent: event.Percent, ReceivedObjects: event.ReceivedObjects,
		TotalObjects: event.TotalObjects, BytesReceived: event.BytesReceived,
	})
}

func progressPhase(line string) string {
	text := strings.ToLower(line)
	for _, phase := range []string{"counting objects", "compressing objects", "receiving objects", "resolving deltas", "checking out files"} {
		if strings.Contains(text, phase) {
			return phase
		}
	}
	return "transfer"
}

func parseProgressCount(value string) (int64, bool) {
	count, err := strconv.ParseInt(strings.ReplaceAll(value, ",", ""), 10, 64)
	return count, err == nil && count >= 0
}

func parseProgressBytes(value, unit string) int64 {
	amount, err := strconv.ParseFloat(value, 64)
	if err != nil || amount < 0 {
		return 0
	}
	multiplier := int64(1)
	switch strings.ToLower(unit) {
	case "byte", "bytes", "b":
	case "kib":
		multiplier = 1 << 10
	case "mib":
		multiplier = 1 << 20
	case "gib":
		multiplier = 1 << 30
	case "tib":
		multiplier = 1 << 40
	case "kb":
		multiplier = 1_000
	case "mb":
		multiplier = 1_000_000
	case "gb":
		multiplier = 1_000_000_000
	case "tb":
		multiplier = 1_000_000_000_000
	default:
		return 0
	}
	return int64(amount * float64(multiplier))
}

func emitProgress(observer func(OperationEvent), event OperationEvent) {
	if observer == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	observer(event)
}

var _ io.Writer = (*progressWriter)(nil)
