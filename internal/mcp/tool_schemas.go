package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `Shared context graph search and recording. Select a graph with graph_list. Before node/relation writes, get agent_id from agent_register. Reuse registration keys per account/OAuth client; registration does not prove identity or grant access.
graph_get.entry_context_id is the entry summary. navigation_refs are navigation; derived_from is evidence. Traverse with node_get and express conversation order through conversation_id, turn_index and previous_event_id.
graph_update/node_update require the current expected_version; reread on conflict. Correct sources with new sources. Change derived evidence/kind with a new derived node and supersedes_context_id.
Registration/retry keys use only ASCII letters, digits, _ or -. request_id is optional; the same key and arguments replay the first result for 24 hours, while a new key is a new write. Reread state if an unkeyed result is uncertain or the key is lost. Do not repeatedly log in for internal failures.
The server checks authentication, grade and agent binding per request; annotations/host approval do not replace authorization. Deletion, permissions and teams are web-only. node_restore restores operation discards; restore web deletions on the web.
Search with context_flow_get. Partial channel failures and truncation may yield normal partial results.`

func toolDefinitions() []*sdk.Tool {
	read := func() *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: new(false), OpenWorldHint: new(false)}
	}
	write := func(destructive, idempotent bool) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: idempotent, DestructiveHint: new(destructive), OpenWorldHint: new(false)}
	}
	tool := func(name, description string, schema *jsonschema.Schema, annotations *sdk.ToolAnnotations) *sdk.Tool {
		return &sdk.Tool{Name: name, Description: description, InputSchema: schema, Annotations: annotations}
	}
	graph := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{"graph_id": idSchema("Graph UUIDv7.")}
	}
	node := func() map[string]*jsonschema.Schema {
		p := graph()
		p["context_id"] = idSchema("Context UUIDv7.")
		return p
	}

	registration := objectSchema(map[string]*jsonschema.Schema{"registration_key": keySchema("Reusable key: 16-128 ASCII letters, digits, _ or -. Never include credentials or content.")}, "registration_key")
	list := objectSchema(map[string]*jsonschema.Schema{
		"name_filter":  textSchema("Case-insensitive name substring.", 0, 0),
		"grade_filter": enumArray("Filter by current grade. Empty and duplicate values allowed.", 3, "owner", "editor", "viewer"),
		"cursor":       cursorSchema(), "page_size": integerSchema("Page size; default and maximum depend on plan.", 1),
	})
	createGraph := objectSchema(map[string]*jsonschema.Schema{
		"name":        nonblankSchema("Display name; nonblank, duplicates allowed.", 200),
		"description": textSchema("Description; may be empty.", 0, 2000), "request_id": requestKeySchema(),
	}, "name")
	updateGraph := graph()
	updateGraph["expected_version"] = versionSchema()
	updateGraph["name"] = nonblankSchema("New nonblank name; omit to keep.", 200)
	updateGraph["description"] = textSchema("New description; omit to keep, empty to clear.", 0, 2000)
	updateGraph["request_id"] = requestKeySchema()
	updateGraph["entry_context_id"] = idSchema("Active entry summary; omit to keep, null to clear.")
	updateGraph["entry_context_id"].Type = ""
	updateGraph["entry_context_id"].Types = []string{"string", "null"}

	createNode := graph()
	createNode["layer"] = enumSchema("Required by layer: source: source_ref, occurred_at, origin_kind; derived: derived_from, derivation_kind, evidence_state; event: member_refs, event_start. Reject fields belonging to other layers.", "source", "derived", "event")
	createNode["body"] = bodySchema()
	actorFields(createNode)
	createNode["source_ref"] = objectSchema(map[string]*jsonschema.Schema{
		"channel": enumSchema("Collection channel, separate from origin_kind.", "conversation", "file", "web", "api", "other"),
		"locator": {Type: "string", Description: "Original URI; the server does not fetch it.", MinLength: new(1), MaxLength: new(1024), Pattern: "^[A-Za-z][A-Za-z0-9+.-]*:", Format: "uri"},
	}, "channel", "locator")
	createNode["source_ref"].Description = "Source deduplication key. Returns existing sources even if discarded/deleted; never restores automatically."
	createNode["occurred_at"] = utcSchema("Original occurrence time in UTC, distinct from recording time.", false)
	createNode["origin_kind"] = enumSchema("Immutable origin category.", "user_utterance", "agent_output", "external_content")
	createNode["conversation_id"] = nonblankSchema("Immutable nonblank conversation ID for sources and events. Sources require channel=conversation and turn_index.", 128)
	createNode["turn_index"] = integerSchema("Immutable conversation source turn order; paired with conversation_id.", 0)
	createNode["previous_event_id"] = idSchema("Event only: active previous event in the same graph and conversation. Requires conversation_id. Atomically creates confirmed precedes; not stored as a property.")
	createNode["navigation_refs"] = refsSchema("Immutable summary navigation, not evidence: events, summaries or sources in the same graph. No self-reference.", 100)
	createNode["navigation_refs"].MinItems = new(0)
	createNode["navigation_refs"].UniqueItems = true
	createNode["derived_from"] = refsSchema("Immutable source/derived evidence references in the same graph.", 100)
	createNode["derivation_kind"] = enumSchema("Immutable derivation purpose.", "proposition", "summary", "reflection")
	createNode["summary_scope"] = enumSchema("Summary scope: required for summary; forbidden otherwise.", "local", "global")
	createNode["evidence_state"] = enumSchema("Immutable evidence category.", "observation", "experience", "opinion")
	createNode["confidence_state"] = enumSchema("Confidence state: required for opinion; forbidden otherwise.", "supported", "uncertain", "disputed")
	createNode["valid_from"] = utcSchema("Derived validity start in UTC; omit for open start.", false)
	createNode["valid_to"] = utcSchema("Derived validity end in UTC; omit for open end.", false)
	createNode["supersedes_context_id"] = idSchema("Prior derived node in the same lineage. Edge: new to prior; separate from evidence.")
	createNode["member_refs"] = refsSchema("Source/derived members in the same graph; no events.", 1000)
	createNode["event_start"] = utcSchema("Event start in UTC. Source members must occur within the event interval.", false)
	createNode["event_end"] = utcSchema("Event end in UTC; omit for ongoing.", false)
	creation := objectSchema(createNode, "graph_id", "layer", "body", "agent_id")
	sourceFields := []string{"source_ref", "occurred_at", "origin_kind"}
	sourceOnlyFields := append(append([]string{}, sourceFields...), "turn_index")
	derivedFields := []string{"derived_from", "derivation_kind", "summary_scope", "evidence_state", "confidence_state", "valid_from", "valid_to", "supersedes_context_id", "navigation_refs"}
	eventFields := []string{"member_refs", "event_start", "event_end", "previous_event_id"}
	creation.AllOf = []*jsonschema.Schema{
		conditional("layer", "source", sourceFields, append(append([]string{}, derivedFields...), eventFields...)),
		conditional("layer", "derived", []string{"derived_from", "derivation_kind", "evidence_state"}, append(append(append([]string{}, sourceOnlyFields...), eventFields...), "conversation_id")),
		conditional("layer", "event", []string{"member_refs", "event_start"}, append(append([]string{}, sourceOnlyFields...), derivedFields...)),
		conditional("derivation_kind", "summary", []string{"summary_scope"}, nil),
		conditional("evidence_state", "opinion", []string{"confidence_state"}, nil),
		{If: &jsonschema.Schema{Required: []string{"turn_index"}}, Then: &jsonschema.Schema{Required: []string{"conversation_id"}}},
		{If: &jsonschema.Schema{AllOf: []*jsonschema.Schema{requiredValue("layer", "source"), {Required: []string{"conversation_id"}}}}, Then: &jsonschema.Schema{Required: []string{"turn_index"}, Properties: map[string]*jsonschema.Schema{"source_ref": {Properties: map[string]*jsonschema.Schema{"channel": {Const: new(any("conversation"))}}}}}},
		{If: &jsonschema.Schema{Required: []string{"previous_event_id"}}, Then: &jsonschema.Schema{Required: []string{"conversation_id"}}},
		{If: &jsonschema.Schema{Not: requiredValue("derivation_kind", "summary")}, Then: forbidFields("summary_scope", "navigation_refs")},
		{If: &jsonschema.Schema{Not: requiredValue("evidence_state", "opinion")}, Then: forbidFields("confidence_state")},
	}

	getNode := node()
	getNode["hops"] = integerSchema("Hop count; default 0 returns only the start. Maximum depends on plan.", 0)
	getNode["direction"] = enumSchema("Default both. out follows derived to evidence/prior, summary to navigation, event to members/relation targets; in reverses.", "in", "out", "both")
	getNode["traversal_filter"] = enumArray("Edge filter; omit or empty for all. Duplicates allowed.", 8, "derived_from", "supersedes", "member_refs", "navigation_refs", "precedes", "causes", "part_of", "relates_to")
	updateNode := node()
	actorFields(updateNode)
	updateNode["expected_version"] = versionSchema()
	updateNode["management_action"] = enumSchema("Default update. keep checks the current version and records judgment without mutable fields.", "update", "keep")
	updateNode["body"] = bodySchema()
	updateNode["confidence_state"] = enumSchema("New confidence for an opinion derived node.", "supported", "uncertain", "disputed")
	updateNode["valid_from"] = utcSchema("New derived validity start in UTC; null to clear.", true)
	updateNode["valid_to"] = utcSchema("New derived validity end in UTC; null to clear.", true)
	updateNode["member_refs"] = refsSchema("Replacement source/derived members; no events.", 1000)
	updateNode["event_start"] = utcSchema("New event start in UTC; source members must occur within the interval.", false)
	updateNode["event_end"] = utcSchema("New event end in UTC; null for ongoing.", true)
	updating := objectSchema(updateNode, "graph_id", "context_id", "expected_version", "agent_id")
	updating.AllOf = []*jsonschema.Schema{conditional("management_action", "keep", nil, []string{"body", "confidence_state", "valid_from", "valid_to", "member_refs", "event_start", "event_end"})}
	discard, restore := node(), node()
	actorFields(discard)
	actorFields(restore)
	flow := graph()
	flow["work_context"] = nonblankSchema("Current task text; nonblank.", 8000)
	flow["as_of"] = utcSchema("Query time in UTC, not historical restoration. Defaults to now.", false)
	flow["scope"] = enumSchema("Search scope; default auto.", "local", "global", "auto")
	flow["budget"] = integerSchema("Response body character budget, not tokens. Default and maximum depend on plan.", 1)
	relations := node()
	relations["state_filter"] = enumArray("State filter; empty and duplicate values allowed.", 3, "proposed", "confirmed", "discarded")
	relations["type_filter"] = enumArray("Type filter; empty and duplicate values allowed.", 4, "precedes", "causes", "part_of", "relates_to")
	relations["cursor"], relations["page_size"] = cursorSchema(), integerSchema("Page size; default and maximum depend on plan.", 1)
	confirm := graph()
	actorFields(confirm)
	confirm["relation_type"] = enumSchema("precedes: earlier to later; causes: cause to effect; part_of: part to whole; relates_to: undirected. Reconfirming the same graph/type/endpoints returns current state.", "precedes", "causes", "part_of", "relates_to")
	confirm["from_context_id"] = idSchema("Active event at the starting endpoint in the same graph. For precedes/causes, it must not start after the target event.")
	confirm["to_context_id"] = idSchema("Active target event in the same graph. For part_of, its interval must contain the source event.")
	discardRelation := graph()
	actorFields(discardRelation)
	discardRelation["relation_id"] = idSchema("Relation UUIDv7 to discard; no repeated discard.")
	return []*sdk.Tool{
		tool("agent_register", "Register an agent. Reusing an active key returns the same ID; revoked keys stay revoked.", registration, write(false, true)),
		tool("graph_list", "List accessible active graphs by recent activity.", list, read()),
		tool("graph_create", "Create a graph owned by the authenticated account.", createGraph, write(false, false)),
		tool("graph_get", "Get graph metadata, current version and entry point.", objectSchema(graph(), "graph_id"), read()),
		tool("graph_update", "Update name, description or entry summary using the current expected_version.", objectSchema(updateGraph, "graph_id", "expected_version"), write(true, false)),
		tool("node_create", "Create a source, derived node or event. A creation_status=existing_deleted result cannot be active evidence. Connect events with previous_event_id or relation_confirm.", creation, write(true, false)),
		tool("node_get", "Get a subgraph via confirmed edges. Truncation is a normal partial result.", objectSchema(getNode, "graph_id", "context_id"), read()),
		tool("node_update", "Update mutable derived/event fields or record a keep judgment. Sources are immutable. Omitted fields stay unchanged.", updating, write(true, false)),
		tool("node_discard", "Soft-discard a node. Reject repeated discard; no cascade.", objectSchema(discard, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("node_restore", "Restore an operation-discarded node. Restore web deletions on the web. Reject already active nodes.", objectSchema(restore, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("context_flow_get", "Retrieve a flat context flow for a task. Channel failures and budget/hop truncation may produce normal partial results.", objectSchema(flow, "graph_id", "work_context"), read()),
		tool("relation_list", "List event relations by state/type, ordered by descending relation_id.", objectSchema(relations, "graph_id", "context_id"), read()),
		tool("relation_confirm", "Confirm/reconfirm relations between active events in one graph. Use derived_from/member_refs for source/derived links. Failure does not undo prior node creation.", objectSchema(confirm, "graph_id", "relation_type", "from_context_id", "to_context_id", "agent_id"), write(true, true)),
		tool("relation_discard", "Discard an event relation. Reject repeated discard; restore with relation_confirm.", objectSchema(discardRelation, "graph_id", "relation_id", "agent_id"), write(true, false)),
	}
}

func objectSchema(properties map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: properties, Required: required, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}

func textSchema(description string, minimum, maximum int) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description, MinLength: new(minimum), Pattern: "^[^\x00]*$"}
	if maximum > 0 {
		s.MaxLength = new(maximum)
	}
	return s
}

func nonblankSchema(description string, maximum int) *jsonschema.Schema {
	s := textSchema(description, 1, maximum)
	s.AllOf = []*jsonschema.Schema{{Pattern: "[^\x09-\x0d\x20\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"}}
	return s
}

func enumSchema(description string, values ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description}
	for _, value := range values {
		s.Enum = append(s.Enum, value)
	}
	return s
}

func idSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description, Format: "uuid-v7", Pattern: "^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-7[0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|[0-9a-fA-F]{12}7[0-9a-fA-F]{3}[89aAbB][0-9a-fA-F]{15})$"}
}

func keySchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description, MinLength: new(16), MaxLength: new(128), Pattern: "^[A-Za-z0-9_-]+$"}
}

func requestKeySchema() *jsonschema.Schema {
	return keySchema("Retry key.")
}

func actorFields(properties map[string]*jsonschema.Schema) {
	properties["agent_id"] = idSchema("Active agent_id.")
	properties["request_id"] = requestKeySchema()
	properties["judgment_input"] = textSchema("Judgment input; omit to record validated arguments.", 1, 8000)
}

func bodySchema() *jsonschema.Schema {
	return textSchema("Body; no NUL, whitespace-only allowed. Stored and indexed verbatim.", 1, 8000)
}
func versionSchema() *jsonschema.Schema {
	return integerSchema("Current retrieved version; reread on conflict.", 1)
}
func integerSchema(description string, minimum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: description, Minimum: new(float64(minimum))}
}
func refsSchema(description string, maximum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: idSchema(""), MinItems: new(1), MaxItems: new(maximum)}
}
func enumArray(description string, maximum int, values ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: enumSchema("", values...), MaxItems: new(maximum)}
}
func utcSchema(description string, nullable bool) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description, Format: "date-time", Pattern: `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|\+00:00)$`}
	if nullable {
		s.Type = ""
		s.Types = []string{"string", "null"}
	}
	return s
}
func cursorSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: "Reuse the returned cursor unchanged. Omit or empty for the first page.", Pattern: "^[A-Za-z0-9_-]*$", Format: "base64url"}
}
func requiredValue(field, value string) *jsonschema.Schema {
	return &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{field: {Const: new(any(value))}}, Required: []string{field}}
}
func forbidFields(fields ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{}
	for _, field := range fields {
		s.AnyOf = append(s.AnyOf, &jsonschema.Schema{Required: []string{field}})
	}
	return &jsonschema.Schema{Not: s}
}
func conditional(field, value string, required, forbidden []string) *jsonschema.Schema {
	then := &jsonschema.Schema{Required: required}
	if len(forbidden) > 0 {
		then.AllOf = []*jsonschema.Schema{forbidFields(forbidden...)}
	}
	return &jsonschema.Schema{If: requiredValue(field, value), Then: then}
}
