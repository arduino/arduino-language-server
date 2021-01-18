package lsp

import (
	"encoding/json"
	"fmt"
)

type Position struct {
	/**
	 * Line position in a document (zero-based).
	 */
	Line int `json:"line"`

	/**
	 * Character offset on a line in a document (zero-based).
	 */
	Character int `json:"character"`
}

func (p Position) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Character)
}

// In returns true if the Position is within the Range r
func (p Position) In(r Range) bool {
	return p.AfterOrEq(r.Start) && p.BeforeOrEq(r.End)
}

// BeforeOrEq returns true if the Position is before or equal the give Position
func (p Position) BeforeOrEq(q Position) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Character <= q.Character)
}

// AfterOrEq returns true if the Position is after or equal the give Position
func (p Position) AfterOrEq(q Position) bool {
	return p.Line > q.Line || (p.Line == q.Line && p.Character >= q.Character)
}

type Range struct {
	/**
	 * The range's start position.
	 */
	Start Position `json:"start"`

	/**
	 * The range's end position.
	 */
	End Position `json:"end"`
}

var NilRange = Range{}

func (r Range) String() string {
	return fmt.Sprintf("%s-%s", r.Start, r.End)
}

// Overlaps returns true if the Range overlaps with the given Range p
func (r Range) Overlaps(p Range) bool {
	return r.Start.In(p) || r.End.In(p) || p.Start.In(r) || p.End.In(r)
}

type Location struct {
	URI   DocumentURI `json:"uri"`
	Range Range       `json:"range"`
}

type Diagnostic struct {
	/**
	 * The range at which the message applies.
	 */
	Range Range `json:"range"`

	/**
	 * The diagnostic's severity. Can be omitted. If omitted it is up to the
	 * client to interpret diagnostics as error, warning, info or hint.
	 */
	Severity DiagnosticSeverity `json:"severity,omitempty"`

	/**
	 * The diagnostic's code. Can be omitted.
	 */
	Code string `json:"code,omitempty"`

	/**
	 * A human-readable string describing the source of this
	 * diagnostic, e.g. 'typescript' or 'super lint'.
	 */
	Source string `json:"source,omitempty"`

	/**
	 * The diagnostic's message.
	 */
	Message string `json:"message"`
}

type DiagnosticSeverity int

const (
	Error       DiagnosticSeverity = 1
	Warning                        = 2
	Information                    = 3
	Hint                           = 4
)

type Command struct {
	/**
	 * Title of the command, like `save`.
	 */
	Title string `json:"title"`
	/**
	 * The identifier of the actual command handler.
	 */
	Command string `json:"command"`
	/**
	 * Arguments that the command handler should be
	 * invoked with.
	 */
	Arguments []json.RawMessage `json:"arguments"`
}

type TextEdit struct {
	/**
	 * The range of the text document to be manipulated. To insert
	 * text into a document create a range where start === end.
	 */
	Range Range `json:"range"`

	/**
	 * The string to be inserted. For delete operations use an
	 * empty string.
	 */
	NewText string `json:"newText"`
}

type WorkspaceEdit struct {
	/**
	 * Holds changes to existing resources.
	 */
	Changes map[DocumentURI][]TextEdit `json:"changes"`
}

type TextDocumentIdentifier struct {
	/**
	 * The text document's URI.
	 */
	URI DocumentURI `json:"uri"`
}

type TextDocumentItem struct {
	/**
	 * The text document's URI.
	 */
	URI DocumentURI `json:"uri"`

	/**
	 * The text document's language identifier.
	 */
	LanguageID string `json:"languageId"`

	/**
	 * The version number of this document (it will strictly increase after each
	 * change, including undo/redo).
	 */
	Version int `json:"version"`

	/**
	 * The content of the opened text document.
	 */
	Text string `json:"text"`
}

type VersionedTextDocumentIdentifier struct {
	TextDocumentIdentifier
	/**
	 * The version number of this document.
	 */
	Version int `json:"version"`
}

type TextDocumentPositionParams struct {
	/**
	 * The text document.
	 */
	TextDocument TextDocumentIdentifier `json:"textDocument"`

	/**
	 * The position inside the text document.
	 */
	Position Position `json:"position"`
}
