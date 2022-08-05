package ls

import (
	"github.com/arduino/go-paths-helper"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (ls *INOLanguageServer) createClangdFormatterConfig(logger jsonrpc.FunctionLogger, cppuri lsp.DocumentURI) (func(), error) {
	// clangd looks for a .clang-format configuration file on the same directory
	// pointed by the uri passed in the lsp command parameters.
	// https://github.com/llvm/llvm-project/blob/64d06ed9c9e0389cd27545d2f6e20455a91d89b1/clang-tools-extra/clangd/ClangdLSPServer.cpp#L856-L868
	// https://github.com/llvm/llvm-project/blob/64d06ed9c9e0389cd27545d2f6e20455a91d89b1/clang-tools-extra/clangd/ClangdServer.cpp#L402-L404

	config := `# Source: https://github.com/arduino/tooling-project-assets/tree/main/other/clang-format-configuration
---
AccessModifierOffset: -2
AlignAfterOpenBracket: Align
AlignArrayOfStructures: None
AlignConsecutiveAssignments: None
AlignConsecutiveBitFields: None
AlignConsecutiveDeclarations: None
AlignConsecutiveMacros: None
AlignEscapedNewlines: DontAlign
AlignOperands: Align
AlignTrailingComments: true
AllowAllArgumentsOnNextLine: true
AllowAllConstructorInitializersOnNextLine: true
AllowAllParametersOfDeclarationOnNextLine: true
AllowShortBlocksOnASingleLine: Always
AllowShortCaseLabelsOnASingleLine: true
AllowShortEnumsOnASingleLine: true
AllowShortFunctionsOnASingleLine: Empty
AllowShortIfStatementsOnASingleLine: AllIfsAndElse
AllowShortLambdasOnASingleLine: Empty
AllowShortLoopsOnASingleLine: true
AlwaysBreakAfterDefinitionReturnType: None
AlwaysBreakAfterReturnType: None
AlwaysBreakBeforeMultilineStrings: false
AlwaysBreakTemplateDeclarations: No
AttributeMacros:
  - __capability
BasedOnStyle: LLVM
BinPackArguments: true
BinPackParameters: true
BitFieldColonSpacing: Both
BraceWrapping:
  AfterCaseLabel: false
  AfterClass: false
  AfterControlStatement: Never
  AfterEnum: false
  AfterFunction: false
  AfterNamespace: false
  AfterObjCDeclaration: false
  AfterStruct: false
  AfterUnion: false
  AfterExternBlock: false
  BeforeCatch: false
  BeforeElse: false
  BeforeLambdaBody: false
  BeforeWhile: false
  IndentBraces: false
  SplitEmptyFunction: true
  SplitEmptyRecord: true
  SplitEmptyNamespace: true
BreakAfterJavaFieldAnnotations: false
BreakBeforeBinaryOperators: NonAssignment
BreakBeforeBraces: Attach
BreakBeforeConceptDeclarations: false
BreakBeforeInheritanceComma: false
BreakBeforeTernaryOperators: true
BreakConstructorInitializers: BeforeColon
BreakConstructorInitializersBeforeComma: false
BreakInheritanceList: BeforeColon
BreakStringLiterals: false
ColumnLimit: 0
CommentPragmas: ''
CompactNamespaces: false
ConstructorInitializerAllOnOneLineOrOnePerLine: false
ConstructorInitializerIndentWidth: 2
ContinuationIndentWidth: 2
Cpp11BracedListStyle: false
DeriveLineEnding: true
DerivePointerAlignment: true
DisableFormat: false
EmptyLineAfterAccessModifier: Leave
EmptyLineBeforeAccessModifier: Leave
ExperimentalAutoDetectBinPacking: false
FixNamespaceComments: false
ForEachMacros:
  - foreach
  - Q_FOREACH
  - BOOST_FOREACH
IfMacros:
  - KJ_IF_MAYBE
IncludeBlocks: Preserve
IncludeCategories:
  - Regex: '^"(llvm|llvm-c|clang|clang-c)/'
    Priority: 2
    SortPriority: 0
    CaseSensitive: false
  - Regex: '^(<|"(gtest|gmock|isl|json)/)'
    Priority: 3
    SortPriority: 0
    CaseSensitive: false
  - Regex: '.*'
    Priority: 1
    SortPriority: 0
    CaseSensitive: false
IncludeIsMainRegex: ''
IncludeIsMainSourceRegex: ''
IndentAccessModifiers: false
IndentCaseBlocks: true
IndentCaseLabels: true
IndentExternBlock: Indent
IndentGotoLabels: false
IndentPPDirectives: None
IndentRequires: true
IndentWidth: 2
IndentWrappedFunctionNames: false
InsertTrailingCommas: None
JavaScriptQuotes: Leave
JavaScriptWrapImports: true
KeepEmptyLinesAtTheStartOfBlocks: true
LambdaBodyIndentation: Signature
Language: Cpp
MacroBlockBegin: ''
MacroBlockEnd: ''
MaxEmptyLinesToKeep: 100000
NamespaceIndentation: None
ObjCBinPackProtocolList: Auto
ObjCBlockIndentWidth: 2
ObjCBreakBeforeNestedBlockParam: true
ObjCSpaceAfterProperty: false
ObjCSpaceBeforeProtocolList: true
PPIndentWidth: -1
PackConstructorInitializers: BinPack
PenaltyBreakAssignment: 1
PenaltyBreakBeforeFirstCallParameter: 1
PenaltyBreakComment: 1
PenaltyBreakFirstLessLess: 1
PenaltyBreakOpenParenthesis: 1
PenaltyBreakString: 1
PenaltyBreakTemplateDeclaration: 1
PenaltyExcessCharacter: 1
PenaltyIndentedWhitespace: 1
PenaltyReturnTypeOnItsOwnLine: 1
PointerAlignment: Right
QualifierAlignment: Leave
ReferenceAlignment: Pointer
ReflowComments: false
RemoveBracesLLVM: false
SeparateDefinitionBlocks: Leave
ShortNamespaceLines: 0
SortIncludes: Never
SortJavaStaticImport: Before
SortUsingDeclarations: false
SpaceAfterCStyleCast: false
SpaceAfterLogicalNot: false
SpaceAfterTemplateKeyword: false
SpaceAroundPointerQualifiers: Default
SpaceBeforeAssignmentOperators: true
SpaceBeforeCaseColon: false
SpaceBeforeCpp11BracedList: false
SpaceBeforeCtorInitializerColon: true
SpaceBeforeInheritanceColon: true
SpaceBeforeParens: ControlStatements
SpaceBeforeParensOptions:
  AfterControlStatements: true
  AfterForeachMacros: true
  AfterFunctionDefinitionName: false
  AfterFunctionDeclarationName: false
  AfterIfMacros: true
  AfterOverloadedOperator: false
  BeforeNonEmptyParentheses: false
SpaceBeforeRangeBasedForLoopColon: true
SpaceBeforeSquareBrackets: false
SpaceInEmptyBlock: false
SpaceInEmptyParentheses: false
SpacesBeforeTrailingComments: 2
SpacesInAngles: Leave
SpacesInCStyleCastParentheses: false
SpacesInConditionalStatement: false
SpacesInContainerLiterals: false
SpacesInLineCommentPrefix:
  Minimum: 0
  Maximum: -1
SpacesInParentheses: false
SpacesInSquareBrackets: false
Standard: Auto
StatementAttributeLikeMacros:
  - Q_EMIT
StatementMacros:
  - Q_UNUSED
  - QT_REQUIRE_VERSION
TabWidth: 2
UseCRLF: false
UseTab: Never
WhitespaceSensitiveMacros:
  - STRINGIZE
  - PP_STRINGIZE
  - BOOST_PP_STRINGIZE
  - NS_SWIFT_NAME
  - CF_SWIFT_NAME
`
	try := func(conf *paths.Path) bool {
		if c, err := conf.ReadFile(); err != nil {
			logger.Logf("    error reading custom formatter config file %s: %s", conf, err)
		} else {
			logger.Logf("    using custom formatter config file %s", conf)
			config = string(c)
		}
		return true
	}

	if sketchFormatterConf := ls.sketchRoot.Join(".clang-format"); sketchFormatterConf.Exist() {
		// If a custom config is present in the sketch folder, use that one
		try(sketchFormatterConf)
	} else if ls.config.FormatterConf != nil && ls.config.FormatterConf.Exist() {
		// Otherwise if a global config file is present, use that one
		try(ls.config.FormatterConf)
	}

	targetFile := cppuri.AsPath()
	if targetFile.IsNotDir() {
		targetFile = targetFile.Parent()
	}
	targetFile = targetFile.Join(".clang-format")
	cleanup := func() {
		targetFile.Remove()
		logger.Logf("    formatter config cleaned")
	}
	logger.Logf("    writing formatter config in: %s", targetFile)
	err := targetFile.WriteFile([]byte(config))
	return cleanup, err
}
