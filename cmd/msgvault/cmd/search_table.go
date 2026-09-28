package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"go.kenn.io/msgvault/internal/textutil"
)

// A zero width means the writer is not a terminal and text stays complete.
func searchTableTerminalWidth(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return 0
	}
	width, _, err := term.GetSize(f.Fd())
	if err != nil || width <= 0 {
		return 80
	}
	return width
}

func normalizeSearchTableText(text string) string {
	return strings.Join(strings.Fields(textutil.SanitizeTerminal(text)), " ")
}

// Markers share the subject allocation but remain visible when text is cut.
type searchTableCell struct {
	text   string
	marker string
}

func (c searchTableCell) display(width int) string {
	if width <= 0 || ansi.StringWidth(c.text+c.marker) <= width {
		return c.text + c.marker
	}
	textWidth := width - ansi.StringWidth(c.marker)
	tail := "..."
	if textWidth < len(tail) {
		tail = ""
	}
	// Iterate entire graphemes, including those beginning with ASCII (keycap
	// emoji). ANSI Truncate's ASCII path can disagree with StringWidth.
	budget := textWidth - ansi.StringWidth(tail)
	var clipped strings.Builder
	used := 0
	for text := c.text; text != ""; {
		cluster, _ := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		cells := ansi.StringWidth(cluster)
		if cells > budget-used {
			break
		}
		clipped.WriteString(cluster)
		used += cells
		text = text[len(cluster):]
	}
	clipped.WriteString(tail)
	clipped.WriteString(c.marker)
	return clipped.String()
}

// All search shapes share ID, DATE, FROM and SUBJECT as their first columns.
// Fixed fields are never truncated, even when a terminal needs to soft wrap.
func writeSearchTable(out io.Writer, headers []string, rows [][]searchTableCell, width int) error {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = ansi.StringWidth(header)
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], ansi.StringWidth(cell.text+cell.marker))
		}
	}
	if width > 0 {
		fixed := 2 * (len(headers) - 1)
		for i, cells := range widths {
			if i != 2 && i != 3 {
				fixed += cells
			}
		}
		available := width - fixed
		widths[2] = max(4, min(widths[2], 30, available-7))
		widths[3] = max(7, min(widths[3], available-widths[2]))
	}

	writeRow := func(cells []string) error {
		var line strings.Builder
		for i, cell := range cells {
			line.WriteString(cell)
			if i < len(cells)-1 {
				line.WriteString(strings.Repeat(" ", widths[i]-ansi.StringWidth(cell)+2))
			}
		}
		if _, err := fmt.Fprintln(out, line.String()); err != nil {
			return fmt.Errorf("write search table row: %w", err)
		}
		return nil
	}
	if err := writeRow(headers); err != nil {
		return err
	}
	rules := make([]string, len(headers))
	for i, header := range headers {
		rules[i] = strings.Repeat("─", ansi.StringWidth(header)/ansi.StringWidth("─"))
	}
	if err := writeRow(rules); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = cell.display(widths[i])
		}
		if err := writeRow(cells); err != nil {
			return err
		}
	}
	return nil
}
