package main

import (
	"context"
	"fmt"

	"github.com/govim/govim"
	"github.com/govim/govim/cmd/govim/config"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/protocol"
	"github.com/govim/govim/cmd/govim/internal/types"
)

// propDict is the representation of arguments used in vim's prop_type_add()
type propDict struct {
	Highlight string `json:"highlight"`
	Combine   bool   `json:"combine,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	StartIncl bool   `json:"start_incl,omitempty"`
	EndIncl   bool   `json:"end_incl,omitempty"`
}

// propAddDict is the representatino of arguments used in vim's prop_add()
type propAddDict struct {
	Type    string `json:"type"`
	ID      int    `json:"id"`
	EndLine int    `json:"end_lnum"`
	EndCol  int    `json:"end_col"` // Column just after the text
	BufNr   int    `json:"bufnr"`
}

func (v *vimstate) textpropDefine() error {
	v.BatchStart()
	// Note that we reuse the highlight name as text property name, even if they aren't the same thing.
	for _, s := range []types.Severity{types.SeverityErr, types.SeverityWarn, types.SeverityInfo, types.SeverityHint} {
		hi := types.SeverityHighlight[s]

		v.BatchChannelCall("prop_type_add", hi, propDict{
			Highlight: string(hi),
			Combine:   true, // Combine with syntax highlight
			Priority:  types.SeverityPriority[s],
		})

		hi = types.SeverityHoverHighlight[s]
		v.BatchChannelCall("prop_type_add", hi, propDict{
			Highlight: string(hi),
			Combine:   true, // Combine with syntax highlight
			Priority:  types.SeverityPriority[s],
		})
	}

	v.BatchChannelCall("prop_type_add", config.HighlightHoverDiagSrc, propDict{
		Highlight: string(config.HighlightHoverDiagSrc),
		Combine:   true, // Combine with syntax highlight
		Priority:  types.SeverityPriority[types.SeverityErr] + 1,
	})

	v.BatchChannelCall("prop_type_add", config.HighlightReferences, propDict{
		Highlight: string(config.HighlightReferences),
		Combine:   true,
		Priority:  types.SeverityPriority[types.SeverityErr] + 1,
	})

	res := v.BatchEnd()
	for i := range res {
		if v.ParseInt(res[i]) != 0 {
			return fmt.Errorf("call to prop_type_add() failed")
		}
	}
	return nil
}

func (v *vimstate) redefineHighlights(diags []types.Diagnostic, force bool) error {
	if v.config.HighlightDiagnostics == nil || !*v.config.HighlightDiagnostics {
		return nil
	}
	v.diagnosticsChangedLock.Lock()
	work := v.diagnosticsChangedHighlights
	v.diagnosticsChangedHighlights = false
	v.diagnosticsChangedLock.Unlock()
	if !force && !work {
		return nil
	}

	v.removeTextProps(types.DiagnosticTextPropID)

	v.BatchStart()
	defer v.BatchCancelIfNotEnded()
	for _, d := range diags {
		// Do not add textprops to unknown buffers
		if d.Buf < 0 {
			continue
		}

		// prop_add() can only be called for Loaded buffers, otherwise
		// it will throw an "unknown line" error.
		if buf, ok := v.buffers[d.Buf]; ok && !buf.Loaded {
			continue
		}

		hi, ok := types.SeverityHighlight[d.Severity]
		if !ok {
			return fmt.Errorf("failed to find highlight for severity %v", d.Severity)
		}

		v.BatchChannelCall("prop_add",
			d.Range.Start.Line(),
			d.Range.Start.Col(),
			propAddDict{string(hi), types.DiagnosticTextPropID, d.Range.End.Line(), d.Range.End.Col(), d.Buf},
		)
	}

	v.BatchEnd()
	return nil
}

func (v *vimstate) updateReferenceHighlight(refresh bool) error {
	if v.config.HighlightReferences == nil || !*v.config.HighlightReferences {
		return nil
	}
	b, pos, err := v.cursorPos()
	if err != nil {
		return fmt.Errorf("failed to get current position: %v", err)
	}

	// refresh indicates if govim should call DocumentHighlight to refresh
	// ranges from gopls since we want to refresh when the user goes idle,
	// and remove highlights as soon as the user is busy. To prevent
	// flickering we keep track of the current highlight range and avoid
	// removing text properties if the cursor is still within the range.
	if !refresh {
		if v.currentReference != nil && pos.IsWithin(*v.currentReference) {
			return nil
		}
		v.currentReference = nil
		v.removeTextProps(types.ReferencesTextPropID)
		return nil
	}

	// Cancel any ongoing requests to make sure that we only process the
	// latest response.
	if v.cancelDocHighlight != nil {
		v.cancelDocHighlight()
		v.cancelDocHighlight = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancelDocHighlight = cancel

	v.tomb.Go(func() error {
		v.redefineReferenceHighlight(ctx, b, pos)
		cancel()
		v.cancelDocHighlight = nil
		return nil
	})

	return nil
}

func (v *vimstate) redefineReferenceHighlight(ctx context.Context, b *types.Buffer, cursorPos types.Point) {
	res, err := v.server.DocumentHighlight(ctx,
		&protocol.DocumentHighlightParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{
					URI: string(b.URI()),
				},
				Position: cursorPos.ToPosition(),
			},
		},
	)

	if err == context.Canceled {
		// A newer request canceled this one, so we shouldn't process the results here
		return
	} else if err != nil {
		v.Logf("documentHighlight call failed: %v", err)
		return
	}

	v.govimplugin.Schedule(func(govim.Govim) error {
		return v.handleDocumentHighlight(b, cursorPos, res)
	})
}

func (v *vimstate) handleDocumentHighlight(b *types.Buffer, cursorPos types.Point, res []protocol.DocumentHighlight) error {
	if v.currentReference != nil {
		v.currentReference = nil
		v.removeTextProps(types.ReferencesTextPropID)
	}

	v.BatchStart()
	defer v.BatchCancelIfNotEnded()
	for i := range res {
		start, err := types.PointFromPosition(b, res[i].Range.Start)
		if err != nil {
			v.Logf("failed to convert start position %v to point: %v", res[i].Range.Start, err)
			return nil
		}
		end, err := types.PointFromPosition(b, res[i].Range.End)
		if err != nil {
			v.Logf("failed to convert end position %v to point: %v", res[i].Range.End, err)
			return nil
		}
		r := types.Range{Start: start, End: end}
		if cursorPos.IsWithin(r) {
			v.currentReference = &r
		}
		v.BatchChannelCall("prop_add",
			start.Line(),
			start.Col(),
			propAddDict{string(config.HighlightReferences), types.ReferencesTextPropID, end.Line(), end.Col(), b.Num},
		)
	}
	v.BatchEnd()
	return nil
}

// removeTextProps is used to remove all added text properties with a specific ID, regardless
// of configuration setting.
func (v *vimstate) removeTextProps(id types.TextPropID) {
	var didStart bool
	if didStart = v.BatchStartIfNeeded(); didStart {
		defer v.BatchCancelIfNotEnded()
	}

	for bufnr, buf := range v.buffers {
		if !buf.Loaded {
			continue // vim removes properties when a buffer is unloaded
		}
		v.BatchChannelCall("prop_remove", struct {
			ID    int `json:"id"`
			BufNr int `json:"bufnr"`
			All   int `json:"all"`
		}{int(id), bufnr, 1})
	}

	if didStart {
		// prop_remove returns number of removed properties per call
		v.BatchEnd()
	}
}
