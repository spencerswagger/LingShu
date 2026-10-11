<script setup lang="ts">
import { computed } from 'vue'
import ChangePasswordForm from './ChangePasswordForm.vue'

const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ (e: 'update:modelValue', v: boolean): void }>()

const visible = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

// 成功提示由表单组件负责，这里只负责关闭弹窗，避免双提示。
function onSuccess() {
  visible.value = false
}
</script>

<template>
  <el-dialog
    v-model="visible"
    title="修改密码"
    width="480px"
    destroy-on-close
    append-to-body
  >
    <ChangePasswordForm @success="onSuccess" />
  </el-dialog>
</template>
