//nolint:wsl_v5 // The role table and the derivation that consumes it stay adjacent.
package tui

import (
	"image/color"

	"github.com/alecthomas/chroma/v2"
)

// A theme whose family ships no bundled chroma style, and every theme loaded
// from a file, gets a complete syntax style derived from its own 14-role
// palette. This file is that derivation: one table maps every chroma token class
// to exactly one palette role, and [derivePaletteStyle] turns the table into a
// self-contained chroma style (no parent, so nothing leaks in from a family
// style the theme does not belong to).
//
// Roles the table deliberately never uses:
//
//   - codeBackground is a surface. A fence draws on the terminal's own canvas,
//     so painting a fill would break the canvas contract the theme system pins
//     (and would colour a light palette's fence inside a dark terminal).
//   - separator and composerPrompt are chrome, not code.
//   - diagnostic is the status panel's own channel, and reusing it in a fence
//     would make a diagnostic colour mean two different things.
//
// Every entry carries its reason, and TestPaletteSyntaxTableIsCompleteAndJustified
// keeps the table complete against the token classes the derivation must cover.
//
// Readability is checked where a fence is actually painted. A fence draws on the
// terminal's own canvas and paints no fill, so a token's contrast is decided by
// the terminal's background, which pips does not know; the palette's accents are
// therefore tuned against the canvas each family was designed for.
// TestBuiltinThemeRepresentativeContrast checks the two pairings that do share
// `code_background` — the fence body and the inline-code pill, both of which are
// the `code`/`workspace` foreground. Measuring the accents against
// `code_background` instead flags most of the shipped catalogue (dracula's muted
// is 1.94:1 there, catppuccin-latte's active 1.70:1), which is a property of each
// family's own palette rather than of this derivation.

// paletteSyntaxRole is the palette role a chroma token class draws its colour
// from. The zero value is the body-text role, so an entry that forgets a role
// still renders readable text rather than an unset colour.
type paletteSyntaxRole uint8

const (
	syntaxWorkspace paletteSyntaxRole = iota
	syntaxMuted
	syntaxModel
	syntaxSession
	syntaxChange
	syntaxActive
	syntaxWarning
	syntaxError
	syntaxCode
)

func (role paletteSyntaxRole) colour(palette colorPalette) color.Color {
	switch role {
	case syntaxMuted:
		return palette.muted
	case syntaxModel:
		return palette.model
	case syntaxSession:
		return palette.session
	case syntaxChange:
		return palette.change
	case syntaxActive:
		return palette.active
	case syntaxWarning:
		return palette.warning
	case syntaxError:
		return palette.error
	case syntaxCode:
		return palette.code
	default:
		return palette.workspace
	}
}

// paletteSyntaxToken is one row of the role -> token-class mapping.
type paletteSyntaxToken struct {
	token     chroma.TokenType
	role      paletteSyntaxRole
	modifiers string
	reason    string
}

// paletteSyntaxTokens is the complete mapping, ordered by token class so the
// table reads like the chroma token tree. The colour semantics it follows:
//
//   - workspace is the transcript's own foreground, so plain code text, names
//     and identifiers read as body text rather than as decoration.
//   - muted carries everything the reader should skim past: comments,
//     punctuation, string delimiters and subordinate headings.
//   - model is the primary accent (headings already use it), so control-flow
//     words, types and markup structure read as structure.
//   - session is the secondary accent (links and the composer cursor already use
//     it), so callables, builtins and prompts read as actions.
//   - change is the value/positive accent, so strings and added diff lines read
//     as values.
//   - active is the attention accent, so numbers and constants read as literals.
//   - warning is the caution accent, so escapes, decorators and entities pop out
//     of the token they sit inside without reading as an error.
//   - error is reserved for failures: exceptions, lexer errors, tracebacks and
//     removed diff lines.
var paletteSyntaxTokens = []paletteSyntaxToken{
	{chroma.Background, syntaxWorkspace, "", "the fence inherits the terminal's canvas, so the style carries no fill"},
	{chroma.Text, syntaxWorkspace, "", "plain code text keeps the transcript's own foreground"},
	{chroma.Error, syntaxError, "", "input that could not be tokenised is a failure"},
	{chroma.Other, syntaxWorkspace, "", "an unclassified token is plain text"},

	{chroma.Keyword, syntaxModel, "", "control-flow words are the primary accent, the one headings already use"},
	{chroma.KeywordConstant, syntaxModel, "", "a keyword-shaped literal stays with the other keywords"},
	{chroma.KeywordDeclaration, syntaxModel, "", "declaration keywords are keywords"},
	{chroma.KeywordNamespace, syntaxModel, "", "import and module keywords are keywords"},
	{chroma.KeywordPseudo, syntaxModel, "", "pseudo keywords are keywords"},
	{chroma.KeywordReserved, syntaxModel, "", "reserved words are keywords"},
	{chroma.KeywordType, syntaxModel, "", "type names read as structure, like the headings that share the accent"},

	{chroma.Name, syntaxWorkspace, "", "a bare identifier is body text"},
	{chroma.NameAttribute, syntaxSession, "", "an attribute is reached through an action accent, like a call"},
	{chroma.NameBuiltin, syntaxSession, "", "a builtin is an action the language provides"},
	{chroma.NameBuiltinPseudo, syntaxSession, "", "self and cls are builtins"},
	{chroma.NameClass, syntaxModel, "", "a class is a type, and types read as structure"},
	{chroma.NameConstant, syntaxActive, "", "a named constant is a literal value"},
	{chroma.NameDecorator, syntaxWarning, "", "a decorator changes the meaning of what follows, so it must pop out"},
	{chroma.NameEntity, syntaxWarning, "", "an entity is a stand-in for a character, so it must pop out of its text"},
	{chroma.NameException, syntaxError, "", "an exception name is a failure"},
	{chroma.NameFunction, syntaxSession, "", "a call is an action"},
	{chroma.NameFunctionMagic, syntaxSession, "", "a magic call is still a call"},
	{chroma.NameKeyword, syntaxModel, "", "a name that acts as a keyword stays with the keywords"},
	{chroma.NameLabel, syntaxSession, "", "a label is a jump target, which reads as an action"},
	{chroma.NameNamespace, syntaxModel, "", "a namespace is structure"},
	{chroma.NameOperator, syntaxSession, "", "a word-shaped operator reads like the operator it stands for"},
	{chroma.NameOther, syntaxWorkspace, "", "an unclassified name is body text"},
	{chroma.NamePseudo, syntaxSession, "", "a pseudo name is a builtin-like action"},
	{chroma.NameProperty, syntaxWorkspace, "", "a property is a name, and names are body text"},
	{chroma.NameTag, syntaxModel, "", "a markup tag is structure"},
	{chroma.NameVariable, syntaxWorkspace, "", "a variable is a name, and names are body text"},
	{chroma.NameVariableAnonymous, syntaxWorkspace, "", "an anonymous variable is still a name"},
	{chroma.NameVariableClass, syntaxWorkspace, "", "a class variable is still a name"},
	{chroma.NameVariableGlobal, syntaxWorkspace, "", "a global variable is still a name"},
	{chroma.NameVariableInstance, syntaxWorkspace, "", "an instance variable is still a name"},
	{chroma.NameVariableMagic, syntaxSession, "", "a magic variable behaves like a builtin"},

	{chroma.Literal, syntaxChange, "", "a bare literal is a value"},
	{chroma.LiteralDate, syntaxActive, "", "a date literal is a literal value"},
	{chroma.LiteralOther, syntaxChange, "", "an unclassified literal is a value"},

	{chroma.LiteralString, syntaxChange, "", "a string is a value, which is what the positive accent means"},
	{chroma.LiteralStringAffix, syntaxChange, "", "a string affix belongs to its string"},
	{chroma.LiteralStringAtom, syntaxChange, "", "an atom is a string-like value"},
	{chroma.LiteralStringBacktick, syntaxChange, "", "a backtick string is a string"},
	{chroma.LiteralStringBoolean, syntaxActive, "", "a boolean literal is a literal value, not text"},
	{chroma.LiteralStringChar, syntaxChange, "", "a character literal is a string"},
	{chroma.LiteralStringDelimiter, syntaxMuted, "", "the quotes are the least informative part of a string"},
	{chroma.LiteralStringDoc, syntaxChange, "", "a docstring is a string, not a comment"},
	{chroma.LiteralStringDouble, syntaxChange, "", "a double-quoted string is a string"},
	{chroma.LiteralStringEscape, syntaxWarning, "", "an escape changes the meaning of the string, so it must pop out"},
	{chroma.LiteralStringHeredoc, syntaxChange, "", "a heredoc is a string"},
	{chroma.LiteralStringInterpol, syntaxWarning, "", "an interpolation is code inside a string, so it must pop out"},
	{chroma.LiteralStringName, syntaxChange, "", "a named string is a string"},
	{chroma.LiteralStringOther, syntaxChange, "", "an unclassified string is a string"},
	{chroma.LiteralStringRegex, syntaxActive, "", "a regex literal is a literal value with its own syntax"},
	{chroma.LiteralStringSingle, syntaxChange, "", "a single-quoted string is a string"},
	{chroma.LiteralStringSymbol, syntaxChange, "", "a symbol literal is a value"},

	{chroma.LiteralNumber, syntaxActive, "", "a number is the literal the reader looks for"},
	{chroma.LiteralNumberBin, syntaxActive, "", "a binary number is a number"},
	{chroma.LiteralNumberFloat, syntaxActive, "", "a float is a number"},
	{chroma.LiteralNumberHex, syntaxActive, "", "a hex number is a number"},
	{chroma.LiteralNumberInteger, syntaxActive, "", "an integer is a number"},
	{chroma.LiteralNumberIntegerLong, syntaxActive, "", "a long integer is a number"},
	{chroma.LiteralNumberOct, syntaxActive, "", "an octal number is a number"},

	{chroma.Operator, syntaxSession, "", "an operator joins values, which is the secondary accent's job"},
	{chroma.OperatorWord, syntaxModel, "", "a word-shaped operator reads as a keyword"},

	{chroma.Punctuation, syntaxMuted, "", "separators carry little meaning, so they recede"},

	{chroma.Comment, syntaxMuted, "", "a comment is deliberately de-emphasised"},
	{chroma.CommentHashbang, syntaxMuted, "", "a hashbang is a comment"},
	{chroma.CommentMultiline, syntaxMuted, "", "a block comment is a comment"},
	{chroma.CommentSingle, syntaxMuted, "", "a line comment is a comment"},
	{chroma.CommentSpecial, syntaxModel, "", "a special comment is a directive, so it reads as structure"},

	{chroma.CommentPreproc, syntaxModel, "", "a preprocessor directive is structure, not prose"},
	{chroma.CommentPreprocFile, syntaxChange, "", "the file a directive names is a value"},

	{chroma.Generic, syntaxWorkspace, "", "an unclassified generic token is body text"},
	{chroma.GenericDeleted, syntaxError, "", "a removed diff line is a failure to keep"},
	{chroma.GenericEmph, syntaxWorkspace, "italic", "emphasis is an attribute, so it keeps the body colour"},
	{chroma.GenericError, syntaxError, "", "a generic error is a failure"},
	{chroma.GenericHeading, syntaxModel, "bold", "a heading is structure and leads its section"},
	{chroma.GenericInserted, syntaxChange, "", "an added diff line is a value gained"},
	{chroma.GenericOutput, syntaxWorkspace, "", "program output is plain text"},
	{chroma.GenericPrompt, syntaxSession, "", "a prompt is the marker of an action"},
	{chroma.GenericStrong, syntaxWorkspace, "bold", "strong text is an attribute, so it keeps the body colour"},
	{chroma.GenericSubheading, syntaxMuted, "bold", "a subordinate heading is structure that should not compete with its parent"},
	{chroma.GenericTraceback, syntaxError, "", "a traceback is a failure"},
	{chroma.GenericUnderline, syntaxWorkspace, "underline", "underlined text is an attribute, so it keeps the body colour"},

	{chroma.TextWhitespace, syntaxWorkspace, "", "whitespace is invisible, so its colour only has to be harmless"},
	{chroma.TextSymbol, syntaxWarning, "", "a symbol stands in for something else, so it must pop out"},
	{chroma.TextPunctuation, syntaxMuted, "", "text punctuation recedes like code punctuation"},
}

// derivePaletteStyle builds a complete, self-contained chroma style from a
// theme's palette. It is a pure function of the palette and the table above, so
// it can be tested without a renderer or a registry, and two calls for the same
// palette produce equal styles.
func derivePaletteStyle(name string, theme colorTheme) (*chroma.Style, error) {
	palette := paletteFor(theme)
	entries := make(chroma.StyleEntries, len(paletteSyntaxTokens))
	for _, row := range paletteSyntaxTokens {
		entry := colorString(row.role.colour(palette))
		if row.modifiers != "" {
			entry += " " + row.modifiers
		}
		entries[row.token] = entry
	}

	style, err := chroma.NewStyle(name, entries)
	if err != nil {
		return nil, err
	}

	return style, nil
}
