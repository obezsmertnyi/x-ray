; Enriched TypeScript/TSX tags query (x-ray-code).
; The upstream tree-sitter-typescript tags.scm only matched signatures and
; interfaces, missing the dominant React/Next forms (function_declaration,
; arrow-function consts, exported components). This adds those, borrowing the
; JS definition patterns (TS is a superset of the JS grammar) plus TS-specific
; type/interface/enum declarations.

; --- Functions ------------------------------------------------------------
(function_declaration
  name: (identifier) @name) @definition.function

(function_signature
  name: (identifier) @name) @definition.function

(generator_function_declaration
  name: (identifier) @name) @definition.function

; const Foo = (...) => {...}  /  const Foo = function(){...}
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function

(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function

; --- Methods --------------------------------------------------------------
(method_definition
  name: (property_identifier) @name) @definition.method

(method_signature
  name: (property_identifier) @name) @definition.method

(abstract_method_signature
  name: (property_identifier) @name) @definition.method

; --- Classes / interfaces / types / enums ---------------------------------
(class_declaration
  name: (type_identifier) @name) @definition.class

(abstract_class_declaration
  name: (type_identifier) @name) @definition.class

(interface_declaration
  name: (type_identifier) @name) @definition.interface

(type_alias_declaration
  name: (type_identifier) @name) @definition.type

(enum_declaration
  name: (identifier) @name) @definition.type

; --- References -----------------------------------------------------------
; Calls and new expressions are not matched here: scriptFacts
; (script_calls.go) emits them with their receiver, which a query cannot see.
