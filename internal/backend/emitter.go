package backend

import "github.com/tyro668/avora/internal/lmbp"

type Emitter struct {
	Emit  func(lmbp.TaskDelta) error
	total int
}

const MaxOutputBytes = 10 << 20

func (l *Emitter) Delta(d lmbp.TaskDelta) error {
	l.total += len(d.Text)
	for _, tc := range d.ToolCalls {
		l.total += len(tc.Function.Arguments) + len(tc.Function.Name)
	}
	if l.total > MaxOutputBytes {
		return Failure("OUTPUT_LIMIT_EXCEEDED")
	}
	for len(d.Text) > 32<<10 {
		n := 32 << 10
		for n > 0 && (d.Text[n]&0xC0) == 0x80 {
			n--
		}
		if err := l.Emit(lmbp.TaskDelta{Text: d.Text[:n]}); err != nil {
			return err
		}
		d.Text = d.Text[n:]
	}
	if d.Text == "" && len(d.ToolCalls) == 0 {
		return nil
	}
	return l.Emit(d)
}

func (l *Emitter) Text(s string) error { return l.Delta(lmbp.TaskDelta{Text: s}) }
