import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  messagesDispatchFormStateToConfig,
  supportsMessagesDispatchPlatform,
} from '../groupsMessagesDispatch'

const source = readFileSync(resolve(process.cwd(), 'src/views/admin/GroupsView.vue'), 'utf8')

describe('GroupsView domestic OpenAI-compatible Messages dispatch', () => {
  it('keeps the Messages mapping controls available for all supported providers', () => {
    expect(['openai', 'kimi', 'zhipu', 'deepseek'].every(supportsMessagesDispatchPlatform)).toBe(true)
    expect(['anthropic', 'gemini', 'grok'].some(supportsMessagesDispatchPlatform)).toBe(false)
    expect(source.match(/v-if="supportsMessagesDispatchPlatform\(/g)).toHaveLength(2)
  })

  it('clears Messages and OAuth-only filters only when a platform does not support them', () => {
    expect(source).toContain('if (!supportsMessagesDispatchPlatform(newVal))')
    expect(source).toContain('!["openai", "antigravity", "anthropic", "gemini", "seedace"].includes(newVal)')
    expect(source).toContain('createForm.require_oauth_only = false')
    expect(source).toContain('editForm.require_privacy_set = false')
  })

  it('persists domestic Messages mapping for both creation and editing', () => {
    expect(source).toContain('supportsMessagesDispatchPlatform(createForm.platform)')
    expect(source).toContain('supportsMessagesDispatchPlatform(editForm.platform)')
    expect(source).not.toContain('if (newVal !== \'openai\')')
  })

  it('serializes family and exact mappings without dropping domestic-provider values', () => {
    expect(messagesDispatchFormStateToConfig({
      allow_messages_dispatch: true,
      opus_mapped_model: ' kimi-k2 ',
      sonnet_mapped_model: ' glm-4.5 ',
      haiku_mapped_model: ' deepseek-chat ',
      exact_model_mappings: [
        { claude_model: ' claude-sonnet-4-5 ', target_model: ' kimi-k2 ' },
        { claude_model: '', target_model: 'ignored' },
      ],
    })).toEqual({
      opus_mapped_model: 'kimi-k2',
      sonnet_mapped_model: 'glm-4.5',
      haiku_mapped_model: 'deepseek-chat',
      exact_model_mappings: { 'claude-sonnet-4-5': 'kimi-k2' },
    })
  })
})
