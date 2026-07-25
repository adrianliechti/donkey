# A Tcl web server — deliberately exotic so neither a Dockerfile nor railpack
# recognizes it. Only the AI detection fallback can figure this one out.
# Serves ./content on the port given by $PORT (default 9090).
set port 9090
if {[info exists env(PORT)]} { set port $env(PORT) }

proc answer {sock addr p} {
    fconfigure $sock -translation crlf
    gets $sock line
    puts $sock "HTTP/1.0 200 OK"
    puts $sock "Content-Type: text/plain"
    puts $sock ""
    puts $sock "legacy tcl app ok"
    close $sock
}

socket -server answer $port
vwait forever
