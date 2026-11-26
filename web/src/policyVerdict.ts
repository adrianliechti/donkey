// How the server words a service policy verdict, read back for display.

const policyPrefix = 'service policy: '

// Whether the message is the policy saying no, rather than failing to answer.
export function isPolicyDenial(message: string): boolean {
  return message.startsWith(policyPrefix)
}

// The server reports every denial of one decision on its own line, after a
// prefix. Anything else (the policy could not be asked) stays one message.
export function policyLines(message: string): string[] {
  if (!isPolicyDenial(message)) return [message]
  return message
    .slice(policyPrefix.length)
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
}

// The few words a row has room for; the whole verdict goes in its title.
export function policyLabel(message: string): string {
  return isPolicyDenial(message) ? 'blocked by policy' : 'policy unavailable'
}
