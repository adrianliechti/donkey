import type { ConsoleProps } from './consoles'
import { ChatView } from './AiChat'
import { ModelsView } from './AiModels'

/**
 * The console of an OpenAI-compatible service: chat with it, or see the
 * models it offers. Both go through the service's console routes, where the
 * server adds the service's key — the browser holds only its session.
 */
export function Console({ space, service, tab }: ConsoleProps) {
  return tab === 'models' ? <ModelsView space={space} service={service} /> : <ChatView space={space} service={service} />
}
