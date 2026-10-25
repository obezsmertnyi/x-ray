(module (expression_statement (assignment left: (identifier) @name) @definition.constant))

(class_definition
  name: (identifier) @name) @definition.class

(function_definition
  name: (identifier) @name) @definition.function

; Calls are not matched here: scriptFacts (script_calls.go) emits them with
; their receiver, which a query cannot see.
