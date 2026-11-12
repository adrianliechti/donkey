import type { ConsoleProps } from './consoles'
import { ChatView } from './AiChat'

/**
 * The console of an OpenAI-compatible service: a quick chat to see that it
 * answers. It goes through the service's console route, where the server
 * adds the service's key — the browser holds only its session.
 */
export function Console({ space, service }: ConsoleProps) {
  return <ChatView space={space} service={service} />
}
