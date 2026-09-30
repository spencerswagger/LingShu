<template>
  <div class="forbidden">
    <el-result icon="warning" title="403" sub-title="暂无权限访问该页面，请联系管理员。">
      <template #extra>
        <el-button type="primary" @click="$router.push('/')">返回首页</el-button>
      </template>
    </el-result>
  </div>
</template>

<style scoped>
.forbidden {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
}
</style>