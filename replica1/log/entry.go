package log

type EntryType string

const (
	EntryTypeStroke EntryType = "STROKE"
	EntryTypeUndo   EntryType = "UNDO_COMPENSATION"
)

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type StrokeData struct {
	Points []Point `json:"points"`
	Colour string  `json:"colour"`
	Width  float64 `json:"width"`
	Tool   string  `json:"tool"` // "pen" | "eraser"
}

type LogEntry struct {
	Index     int64      `json:"index"`
	Term      int64      `json:"term"`
	Type      EntryType  `json:"type"`
	StrokeID  string     `json:"strokeId"`
	UserID    string     `json:"userId"`
	Data      StrokeData `json:"data"`
	Timestamp int64      `json:"ts"`
}
