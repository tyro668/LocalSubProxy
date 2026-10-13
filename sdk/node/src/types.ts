export interface TenantProfile {
  display_name?: string;
  logo_uri?: string;
  homepage_uri?: string;
  policy_uri?: string;
  tos_uri?: string;
  pairing_uri?: string;
  requested_models?: string[];
  requested_scopes?: string[];
}
export interface TenantConfig { id: string; /** >= 32 chars; server-side only */ apiKey: string; profile?: TenantProfile }
export interface PairingInfo { link: string; qrSVG: string; qrDataURL: string }
export interface PendingPairing { userCode: string; deviceName: string; jkt: string; status: string; expiresAt: Date }
export interface ApproveResult { status: 'approved' | 'denied'; deviceId: string; deviceName: string; jkt: string }
export interface DeviceInfo {
  deviceId: string; name: string; status: string; createdAt: Date; jkt: string; online: boolean;
  models: string[]; modelCapabilities: Record<string, string[]>;
  /** Ed25519 identity key (check against your pinned jkt) and the device-signed X25519 key statement ('' if none), for sealE2EERequest. */
  publicKey: Buffer; e2eeKey: string;
}

// ---- OpenAI-compatible shapes (subset) ----
export type ContentPart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string } }
  | { type: 'input_audio'; input_audio: { data: string; format: string } };
export interface ToolCall { id?: string; index?: number; type: 'function'; function: { name: string; arguments: string } }
export interface ChatMessage {
  role: 'system' | 'developer' | 'user' | 'assistant' | 'tool';
  content: string | ContentPart[] | null;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
  name?: string;
}
export interface ChatTool { type: 'function'; function: { name: string; description?: string; parameters?: unknown } }
export interface ChatRequest {
  model: string; messages?: ChatMessage[]; tools?: ChatTool[]; tool_choice?: unknown;
  max_tokens?: number; temperature?: number; stream?: boolean;
  /** Sealed request (sealE2EERequest().jwe); messages/tools must then be empty. */
  lmbp_e2ee?: string;
}
/** One sealed response frame of an E2EE task (open with E2EESealed.open). */
export interface E2EEFrameWire { seq: number; type: 'task.delta' | 'task.done'; data: string }
export interface Usage { prompt_tokens: number; completion_tokens: number; total_tokens?: number }
export interface ChatCompletion {
  id: string; object: 'chat.completion'; created: number; model: string;
  choices: { index: number; message: ChatMessage; finish_reason: string }[]; usage?: Usage;
  lmbp_e2ee?: E2EEFrameWire[];
}
export interface ChatCompletionChunk {
  id: string; object: 'chat.completion.chunk'; created: number; model: string;
  choices: { index: number; delta: Partial<ChatMessage>; finish_reason: string | null }[]; usage?: Usage;
  lmbp_e2ee?: E2EEFrameWire;
}
export interface Model { id: string; object: 'model'; owned_by: string; capabilities: string[] }

// ---- Anthropic Messages shapes (subset) ----
export type AnthropicBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; source: { type: 'base64'; media_type: string; data: string } | { type: 'url'; url: string } }
  | { type: 'tool_use'; id: string; name: string; input: unknown }
  | { type: 'tool_result'; tool_use_id: string; content?: string | { type: 'text'; text: string }[]; is_error?: boolean };
export interface MessagesRequest {
  model: string; max_tokens: number; messages: { role: 'user' | 'assistant'; content: string | AnthropicBlock[] }[];
  system?: string | { type: 'text'; text: string }[];
  tools?: { name: string; description?: string; input_schema: unknown }[];
  tool_choice?: { type: 'auto' | 'any' | 'none' } | { type: 'tool'; name: string };
  temperature?: number; stream?: boolean;
}
export interface Message {
  id: string; type: 'message'; role: 'assistant'; model: string;
  content: ({ type: 'text'; text: string } | { type: 'tool_use'; id: string; name: string; input: any })[];
  stop_reason: 'end_turn' | 'max_tokens' | 'tool_use'; stop_sequence: null;
  usage: { input_tokens: number; output_tokens: number };
}
export interface MessageStreamEvent { type: string; [k: string]: any }

/** Error returned by the in-process client. */
export interface APIErrorShape { status: number; code: string; message: string }
