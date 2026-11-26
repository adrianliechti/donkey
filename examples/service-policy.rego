package donkey.services

levels := {"C1": 1, "C2": 2, "C3": 3, "C4": 4}

classifications(service) := {tag |
	some raw in service.tags
	tag := upper(raw)
	levels[tag]
}

deny contains sprintf("%s needs exactly one classification", [service.name]) if {
	some service in input.services
	count(classifications(service)) != 1
}

deny contains sprintf("%s needs a read or write tag", [service.name]) if {
	some service in input.services
	not "read" in service.tags
	not "write" in service.tags
}

deny contains sprintf("%s reads %s, so %s cannot write %s", [source.name, high, sink.name, low]) if {
	some source in input.services
	"read" in source.tags
	some sink in input.services
	"write" in sink.tags
	some high in classifications(source)
	some low in classifications(sink)
	levels[high] > levels[low]
}

# This source also requires every write destination to check delegated permissions.
deny contains sprintf("%s requires delegated authorization on write destination %s", [source.name, sink.name]) if {
	some source in input.services
	source.id == "case-files"
	"read" in source.tags
	some sink in input.services
	"write" in sink.tags
	sink.authorization != "delegated"
}
