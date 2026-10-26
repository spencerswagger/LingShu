<script setup lang="ts">
import { computed } from 'vue'
import type { MsgBubble } from '@/utils/messageParts'

const props = defineProps<{ bubble: MsgBubble }>()

const isImageUrl = computed(() => {
  const u = props.bubble.url || ''
  return /^(https?:\/\/|data:image\/)/i.test(u)
})
// 较长正文自动折叠，避免系统注入块占满页面。
// 折叠只是隐藏，**内容完整保留**（展开后全文可见，不做任何截断）：
// 账单详情与会话详情要求「长消息可折叠但内容不截断」。
const foldable = computed(() => props.bubble.text.length > 600)
// 纯文本正文的折叠阈值更宽松（1500 字），短文本直接展开，避免气泡里到处都是折叠条。
const longText = computed(() => props.bubble.kind === 'text' && props.bubble.text.length > 1500)
const isToolLike = computed(() => props.bubble.kind === 'tool' || props.bubble.kind === 'json' || (props.bubble.kind === 'xml' && foldable.value))
</script>

<template>
  <div class="c-bubble" :class="`kind-${bubble.kind}`">
    <template v-if="bubble.kind === 'text'">
      <div v-if="longText" class="c-fold">
        <el-collapse>
          <el-collapse-item name="text-0" :title="`正文（${bubble.text.length} 字，点击展开全文）`">
            <div class="c-text c-text-full">{{ bubble.text }}</div>
          </el-collapse-item>
        </el-collapse>
      </div>
      <div v-else class="c-text">{{ bubble.text }}</div>
    </template>

    <template v-else-if="bubble.kind === 'xml'">
      <div v-if="isToolLike" class="c-fold">
        <el-collapse>
          <el-collapse-item :name="'x-' + bubble.tag" :title="`&lt;${bubble.tag}&gt;（${bubble.text.length} 字）`">
            <pre class="c-pre">{{ bubble.text }}</pre>
          </el-collapse-item>
        </el-collapse>
      </div>
      <template v-else>
        <div class="c-tag"># {{ bubble.tag }}</div>
        <div class="c-text">{{ bubble.text }}</div>
      </template>
    </template>

    <template v-else-if="bubble.kind === 'image'">
      <div class="c-tag"># image</div>
      <img
        v-if="isImageUrl"
        :src="bubble.url"
        class="c-img"
        loading="lazy"
        referrerpolicy="no-referrer"
        alt="图片"
      />
      <a v-else :href="bubble.url" target="_blank" rel="noopener noreferrer" class="c-link mono">
        {{ bubble.url }}
      </a>
    </template>

    <template v-else>
      <el-collapse class="c-fold">
        <el-collapse-item
          :name="bubble.kind + '-0'"
          :title="bubble.kind === 'tool' ? '工具调用' : '原始结构'"
        >
          <pre class="c-pre">{{ bubble.text }}</pre>
        </el-collapse-item>
      </el-collapse>
    </template>
  </div>
</template>

<style scoped>
.c-bubble {
  min-width: 0;
  max-width: 100%;
}
.c-text {
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.c-text-full {
  max-height: 420px;
  overflow: auto;
}
.c-tag {
  display: inline-block;
  font-size: 11px;
  line-height: 1;
  color: var(--color-text-tertiary);
  background: var(--color-fill, #f6f7fa);
  border: 1px solid var(--color-border);
  border-radius: 4px;
  padding: 2px 6px;
  margin-bottom: 6px;
}
.c-pre {
  margin: 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 240px;
  overflow: auto;
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.c-img {
  display: block;
  max-width: 100%;
  max-height: 320px;
  border-radius: 8px;
}
.c-link {
  font-size: 12px;
  color: var(--color-primary, #409eff);
  word-break: break-all;
}
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
</style>