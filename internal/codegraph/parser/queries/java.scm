(class_declaration
  name: (identifier) @name) @definition.class

(method_declaration
  name: (identifier) @name) @definition.method

; Calls are not matched here: javaCalls (java_calls.go) emits them with the
; receiver's declared type, which a query cannot see.

(interface_declaration
  name: (identifier) @name) @definition.interface

(type_list
  (type_identifier) @name) @reference.implementation

(object_creation_expression
  type: (type_identifier) @name) @reference.class

(superclass (type_identifier) @name) @reference.class

; Not in the grammar's upstream tags.scm: the facts xray links services from.
; Annotations with their arguments (@FeignClient(name = …, url = "${…}")), the
; declared types a class works with (a field, a parameter, a local, a type
; argument), and imports — the evidence a type name resolves on.

(marker_annotation
  name: (identifier) @name) @reference.annotation

(annotation
  name: (identifier) @name
  arguments: (annotation_argument_list) @args) @reference.annotation

(field_declaration
  type: (type_identifier) @name) @reference.type

(formal_parameter
  type: (type_identifier) @name) @reference.type

(local_variable_declaration
  type: (type_identifier) @name) @reference.type

(type_arguments
  (type_identifier) @name) @reference.type

(import_declaration
  (scoped_identifier) @name) @reference.import
