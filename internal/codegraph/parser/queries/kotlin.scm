; The Kotlin grammar ships no tags.scm, so this one is ours. It names the same
; things the other languages' queries do: type and function definitions, and
; the calls the linker resolves.

(class_declaration
  name: (identifier) @name) @definition.class

(object_declaration
  name: (identifier) @name) @definition.class

(function_declaration
  name: (identifier) @name) @definition.function

(call_expression
  (identifier) @name) @reference.call

(call_expression
  (navigation_expression
    (identifier) @name .)) @reference.call
