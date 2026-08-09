package cardkit

// MarkdownElement builds a markdown body element.
// In card JSON 2.0 the markdown component carries content directly;
// the v1 nested {"text":{"tag":"lark_md"}} wrapper is rejected.
func MarkdownElement(text string) Element {
	return Element{
		"tag":     "markdown",
		"content": text,
	}
}

// HrElement builds a horizontal-rule divider. Used to separate the tool /
// text zones on a progress card so adjacent markdown blocks don't run
// together when several are non-empty.
func HrElement() Element {
	return Element{"tag": "hr"}
}

// callbackBehaviors declares a click that ships the action back to the
// backend. Card JSON 2.0 requires this explicit behaviors declaration for
// interactive callbacks. Schema 1.0 does NOT tolerate it: the platform
// strict-validates v1 buttons and, when the unknown field is present, stops
// classifying the button as a form's submit button (230099 "there is no
// submit button in the form container"). So behaviors is added only on the
// v2 path; v1 buttons keep the legacy shape.
func callbackBehaviors() []map[string]any {
	return []map[string]any{{"type": "callback", "callback": map[string]any{}}}
}

// maybeBehaviors returns the v2 behaviors declaration, or nil under schema 1.0
// so the field is omitted entirely (not emitted as null) on the legacy path.
func maybeBehaviors() []map[string]any {
	if schemaV2.Load() {
		return callbackBehaviors()
	}
	return nil
}

// ButtonAction builds a button action. actionType is stored in value.kind
// so the dispatcher can route the click (permission/question/submit/cancel).
// primary controls the button style; disabled greys it out (R4). behaviors is
// the v2-required callback declaration (harmless under v1).
func ButtonAction(label, actionType string, value map[string]any, primary bool, disabled bool) Action {
	if value == nil {
		value = map[string]any{}
	}
	value["kind"] = actionType
	btn := map[string]any{
		"tag":      "button",
		"text":     map[string]any{"tag": "plain_text", "content": label},
		"type":     "default",
		"value":    value,
		"disabled": disabled,
	}
	if b := maybeBehaviors(); b != nil {
		btn["behaviors"] = b
	}
	if primary {
		btn["type"] = "primary"
	}
	return btn
}

// FormElement wraps interactive components (select/input/button) in a form
// container so an embedded submit button collects every named component's
// value into action.form_value on click. name is the form's card-global
// identifier; each inner component carries its own name as the form_value key.
func FormElement(name string, elements []Element) Element {
	return Element{
		"tag":      "form",
		"name":     name,
		"elements": elements,
	}
}

// SubmitButtonAction builds a button that triggers form submission. On click
// Feishu returns value as action.value (for requestID routing) and the form's
// component values as action.form_value. primary controls the button style.
// form_submit already implies a callback round-trip; behaviors is added only
// on the v2 path (schema 1.0 rejects the unknown field — see maybeBehaviors).
func SubmitButtonAction(label string, value map[string]any, primary bool) Action {
	if value == nil {
		value = map[string]any{}
	}
	btn := map[string]any{
		"tag":         "button",
		"text":        map[string]any{"tag": "plain_text", "content": label},
		"type":        "default",
		"name":        "submit",
		"action_type": "form_submit",
		"value":       value,
	}
	if b := maybeBehaviors(); b != nil {
		btn["behaviors"] = b
	}
	if primary {
		btn["type"] = "primary"
	}
	return btn
}

// SelectOption builds one option for a select/multi-select element. value is
// what form_value returns on selection; label is what the user sees.
func SelectOption(label, value string) map[string]any {
	return map[string]any{
		"text":  map[string]any{"tag": "plain_text", "content": label},
		"value": value,
	}
}

// SelectStaticElement builds a single-select dropdown. multiple=true switches
// to multi_select_static so the user can pick more than one option. name is
// the form_value key; placeholder is the grey prompt shown before selection.
func SelectStaticElement(name, placeholder string, options []map[string]any, multiple bool) Element {
	tag := "select_static"
	if multiple {
		tag = "multi_select_static"
	}
	return Element{
		"tag":         tag,
		"name":        name,
		"placeholder": map[string]any{"tag": "plain_text", "content": placeholder},
		"options":     options,
	}
}

// InputElement builds a free-text input box. name is the form_value key;
// placeholder is the grey prompt shown before the user types.
func InputElement(name, placeholder string) Element {
	return Element{
		"tag":         "input",
		"name":        name,
		"placeholder": map[string]any{"tag": "plain_text", "content": placeholder},
	}
}
