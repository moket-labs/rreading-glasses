package internal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/require"
)

var errMaintenanceNewline = errors.New("maintenance newline write refused")

// The controller's denorm paths use ConfigStd.NewEncoder with a MultiWriter.
// A truncated final newline must not be acknowledged as a successful encode.
type maintenanceNewlineWriter struct {
	bytes.Buffer
}

func (w *maintenanceNewlineWriter) Write(p []byte) (int, error) {
	if bytes.Equal(p, []byte{'\n'}) {
		return 0, errMaintenanceNewline
	}
	return w.Buffer.Write(p)
}

func TestMaintenanceSonicNewlineError(t *testing.T) {
	var writer maintenanceNewlineWriter
	err := sonic.ConfigStd.NewEncoder(&writer).Encode(workResource{})
	require.ErrorIs(t, err, errMaintenanceNewline)
	require.NotEmpty(t, writer.Bytes(), "JSON payload should have reached the writer before the newline failure")
}

// Exercise the same typed resources and stream API used by denormalization,
// including identifiers above JSON's exact-float integer range and GC pressure.
func TestMaintenanceSonicResourceCompatibility(t *testing.T) {
	work := workResource{
		ForeignID:     9007199254740993,
		Title:         "Éditions 日本語 <title>",
		Genres:        []string{},
		Books:         []bookResource{{ForeignID: 9007199254740995, Title: "中文", Description: "line\nquote\"", AverageRating: 4.125}},
		AverageRating: 4.25,
	}
	author := AuthorResource{ForeignID: 9007199254740997, Name: "Zoë 日本語", Works: []workResource{work}}
	for range 32 {
		var buf bytes.Buffer
		encoder := sonic.ConfigStd.NewEncoder(&buf)
		require.NoError(t, encoder.Encode(work))
		require.NoError(t, encoder.Encode(author))
		decoder := sonic.ConfigStd.NewDecoder(&buf)
		var decodedWork workResource
		var decodedAuthor AuthorResource
		require.NoError(t, decoder.Decode(&decodedWork))
		require.NoError(t, decoder.Decode(&decodedAuthor))
		require.ErrorIs(t, decoder.Decode(new(workResource)), io.EOF)
		require.Equal(t, work, decodedWork)
		require.Equal(t, author, decodedAuthor)
		want, err := json.Marshal(author)
		require.NoError(t, err)
		got, err := sonic.ConfigStd.Marshal(author)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
		var direct AuthorResource
		require.NoError(t, sonic.ConfigStd.Unmarshal(want, &direct))
		require.Equal(t, author, direct)
		runtime.GC()
	}
}
