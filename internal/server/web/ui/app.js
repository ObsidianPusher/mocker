const state = {
  currentPath: '',
  selectedFile: '',
  selectedEncoding: 'utf-8',
  noticeTimer: null,
  serviceViews: [],
  roots: [],
  loggedIn: false,
  user: '',
  refreshTimer: null,
  isEditingConfig: false,
  configDirty: false,
  isEditingFile: false,
  fileDirty: false,
  fileTree: { nodes: new Map(), roots: [] },
  wrapEnabled: false,
  isEditingSystem: false,
  systemDirty: false,
  httpAddress: '',
  certFile: '',
  keyFile: '',
  memoryLimitMB: 128
};

async function fetchJSON(url, init) {
  const response = await fetch(url, init);
  if (response.status === 401) {
    handleUnauthorized();
    throw new Error('unauthorized');
  }
  if (!response.ok) {
    const text = await response.text();
    throw new Error(text || response.statusText);
  }
  return response.json();
}

function setNotice(message, type = 'info') {
  const notice = document.getElementById('notice');
  notice.textContent = message;
  notice.dataset.type = type;
  notice.classList.add('visible');
  clearTimeout(state.noticeTimer);
  state.noticeTimer = setTimeout(() => notice.classList.remove('visible'), 4000);
}

function showLogin(message) {
  document.getElementById('login-view').classList.remove('hidden');
  document.getElementById('app-view').classList.add('hidden');
  const error = document.getElementById('login-error');
  error.textContent = message || '';
  stopPolling();
}

function showApp() {
  document.getElementById('login-view').classList.add('hidden');
  document.getElementById('app-view').classList.remove('hidden');
  document.getElementById('current-user').textContent = state.user || '公开模式';
  document.getElementById('account-username').textContent = state.user || '公开模式';
  startPolling();
}

function handleUnauthorized() {
  if (state.loggedIn) {
    state.loggedIn = false;
    state.user = '';
  }
  showLogin('请先登录');
}

function startPolling() {
  if (state.refreshTimer) {
    return;
  }
  refreshState();
  initFileTree();
  state.refreshTimer = setInterval(refreshState, 5000);
}

function stopPolling() {
  if (state.refreshTimer) {
    clearInterval(state.refreshTimer);
    state.refreshTimer = null;
  }
}

async function refreshState() {
  try {
    const payload = await fetchJSON('/api/state');
    state.serviceViews = payload.services || [];
    const nextRoots = payload.roots || [];
    const rootsChanged = JSON.stringify(nextRoots) !== JSON.stringify(state.roots);
    state.roots = nextRoots;
    renderServices(state.serviceViews);
    renderMetrics(payload.metrics);
    renderSystemConfig(payload.http, payload.system);
    if (rootsChanged) {
      ensureRootNodes();
      renderFileTree();
    }
    if (!state.isEditingConfig && !state.configDirty) {
      renderConfigCards();
    }
    if (!state.isEditingConfig) {
      document.getElementById('config-service-list')._configSignature = '';
    }
  } catch (err) {
    setNotice('状态更新失败：' + err.message, 'error');
  }
}

function renderMetrics(metrics) {
  if (!metrics) {
    return;
  }
  const box = document.getElementById('metrics-total');
  const cpuPercent = (metrics.system && metrics.system.cpu_percent) || metrics.total?.cpu_percent || 0;
  box.querySelector('[data-cpu]').textContent = cpuPercent.toFixed(1) + '%';
  const memTotal = metrics.system && metrics.system.memory ? metrics.system.memory.total : 0;
  const memUsed = metrics.system && metrics.system.memory ? metrics.system.memory.used : 0;
  const memAvailable = metrics.system && metrics.system.memory ? metrics.system.memory.available : 0;
  const aggregateMem = metrics.total ? metrics.total.memory_bytes : 0;
  const mainMem = memTotal ? `${humanBytes(memUsed)} / ${humanBytes(memTotal)}` : humanBytes(aggregateMem);
  box.querySelector('[data-mem]').textContent = mainMem;
  box.querySelector('[data-mem-system]').textContent = `系统内存: ${humanBytes(memUsed)} / ${humanBytes(memTotal)}`;
  box.querySelector('[data-mem-services]').textContent = `服务占用: ${humanBytes(aggregateMem)} 可用: ${humanBytes(memAvailable)}`;
  const uploadBps = metrics.system?.network?.upload_bytes_per_sec || 0;
  const downloadBps = metrics.system?.network?.download_bytes_per_sec || 0;
  const downEl = box.querySelector('[data-net-down]');
  if (downEl) {
    downEl.textContent = `下行速率: ${humanRate(downloadBps)}`;
  }
  const upEl = box.querySelector('[data-net-up]');
  if (upEl) {
    upEl.textContent = `上行速率: ${humanRate(uploadBps)}`;
  }
}

function renderSystemConfig(http, system) {
  const address = http?.address || state.httpAddress || '';
  const certFile = http?.cert_file || state.certFile || '';
  const keyFile = http?.key_file || state.keyFile || '';
  const memoryLimit = Number(system?.memory_limit_mb || state.memoryLimitMB || 128);
  if (!address) {
    return;
  }
  state.httpAddress = address;
  state.certFile = certFile;
  state.keyFile = keyFile;
  state.memoryLimitMB = memoryLimit;
  if (state.isEditingSystem || state.systemDirty) {
    return;
  }
  document.getElementById('http-host').textContent = address || '-';
  const memoryEl = document.getElementById('mocker-memory-limit');
  if (memoryEl) {
    memoryEl.value = String(memoryLimit);
  }
  const certEl = document.getElementById('http-cert-file');
  if (certEl) {
    certEl.value = certFile;
  }
  const keyEl = document.getElementById('http-key-file');
  if (keyEl) {
    keyEl.value = keyFile;
  }
}

function renderServices(serviceViews) {
  const container = document.getElementById('service-list');
  const logStates = captureLogStates();
  const previous = container._serviceSignature || '';
  const nextSignature = JSON.stringify(serviceViews.map(view => ({
    id: view?.config?.id || '',
    name: view?.config?.name || '',
    path: view?.config?.path || '',
    running: Boolean(view?.status?.running),
    pid: view?.status?.pid || 0,
    restarts: view?.status?.restarts || 0,
    healthy: view?.status?.healthy,
    memory: view?.status?.metrics?.memory_bytes || 0,
    cpu: view?.status?.metrics?.cpu_percent || 0
  })));
  if (previous === nextSignature) {
    restoreLogStates(logStates);
    return;
  }
  container._serviceSignature = nextSignature;
  container.innerHTML = '';
  if (!serviceViews.length) {
    container.innerHTML = '<p class="muted">暂无服务配置，请在配置页添加</p>';
    return;
  }
  for (const view of serviceViews) {
    const status = view.status || {};
    const pathDescriptor = view.relative_path ? `${view.root || 'root'}/${view.relative_path}` : view.config.path;
    const healthy = status.healthy !== undefined ? status.healthy : status.running;
    const badgeText = healthy ? '正常' : '未运行';
    const badgeClass = healthy ? 'badge-success' : 'badge-warn';
    const portLabel = status.port ? String(status.port) : '未监听';
    const portStateLabel = status.port_open ? '监听中' : '未监听';
    const safeID = escapeHtml(view.config.id || '');
    const safeName = escapeHtml(view.config.name || view.config.id || '');
    const safePath = escapeHtml(pathDescriptor || '');
    const safeLastError = status.last_exit_error ? escapeHtml('上次错误：' + status.last_exit_error) : '';
    const card = document.createElement('article');
    card.className = 'service-card';
    card.dataset.health = healthy ? 'healthy' : 'unhealthy';
    card.innerHTML = `
      <header>
        <div class="service-title">
          <strong class="service-name">${safeName}</strong>
          <p class="muted service-path">${safePath}</p>
        </div>
        <span class="badge ${badgeClass}">${badgeText}</span>
      </header>
      <div class="service-meta">
        <div class="meta-item"><span class="meta-label">PID</span><span class="meta-value">${status.pid || '-'}</span></div>
        <div class="meta-item"><span class="meta-label">重启</span><span class="meta-value">${status.restarts || 0}</span></div>
        <div class="meta-item"><span class="meta-label">端口</span><span class="meta-value">${portLabel}</span></div>
        <div class="meta-item"><span class="meta-label">监听</span><span class="meta-value">${portStateLabel}</span></div>
        <div class="meta-item"><span class="meta-label">CPU</span><span class="meta-value">${status.metrics?.cpu_percent?.toFixed(1) || '0.0'}%</span></div>
        <div class="meta-item"><span class="meta-label">内存</span><span class="meta-value">${humanBytes(status.metrics?.memory_bytes || 0)}</span></div>
      </div>
      <div class="service-actions">
        <button data-action="start" data-id="${safeID}" ${status.running ? 'disabled' : ''}>启动</button>
        <button data-action="stop" data-id="${safeID}" ${status.running ? '' : 'disabled'}>停止</button>
        <button data-action="restart" data-id="${safeID}">重启</button>
        <button data-action="log" data-id="${safeID}">日志</button>
      </div>
      <div class="service-status-details">
        <p class="muted">${safeLastError}</p>
      </div>
      <pre id="log-${safeID}" class="service-log" aria-live="polite"></pre>
    `;
    bindServiceButtons(card, view.config.id);
    container.appendChild(card);
  }
  restoreLogStates(logStates);
}

function captureLogStates() {
  const states = {};
  document.querySelectorAll('.service-log').forEach(el => {
    states[el.id] = {
      text: el.textContent,
      open: el.classList.contains('open'),
      loaded: el.dataset.loaded === 'true',
      scroll: el.scrollTop
    };
  });
  return states;
}

function restoreLogStates(states) {
  Object.entries(states).forEach(([id, info]) => {
    const el = document.getElementById(id);
    if (!el) {
      return;
    }
    el.textContent = info.text || '';
    el.classList.toggle('open', info.open);
    if (info.open) {
      el.scrollTop = info.scroll || el.scrollHeight;
    }
    el.dataset.loaded = info.loaded ? 'true' : '';
  });
}

function bindServiceButtons(card, id) {
  card.querySelector('[data-action="start"]').addEventListener('click', () => controlService(id, 'start'));
  card.querySelector('[data-action="stop"]').addEventListener('click', () => controlService(id, 'stop'));
  card.querySelector('[data-action="restart"]').addEventListener('click', () => controlService(id, 'restart'));
  const logButton = card.querySelector('[data-action="log"]');
  const logArea = card.querySelector('.service-log');
  logButton.addEventListener('click', async () => {
    if (logArea.dataset.loaded === 'true') {
      logArea.classList.toggle('open');
      return;
    }
    try {
      logArea.textContent = '加载日志…';
      const payload = await fetchJSON(`/api/logs/${id}?lines=120`);
      logArea.textContent = payload.log || '日志为空';
      logArea.dataset.loaded = 'true';
      logArea.classList.add('open');
      logArea.scrollTop = logArea.scrollHeight;
    } catch (err) {
      setNotice('日志加载失败：' + err.message, 'error');
      logArea.textContent = '';
    }
  });
}

async function controlService(id, action) {
  const actionLabel = {
    start: '启动',
    stop: '停止',
    restart: '重启'
  };
  try {
    await fetchJSON(`/api/services/${id}/${action}`, { method: 'POST' });
    setNotice(`${actionLabel[action] || action} ${id} 请求提交`, 'success');
    await refreshState();
  } catch (err) {
    setNotice('动作失败：' + err.message, 'error');
  }
}

function humanBytes(bytes) {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes || 0;
  let idx = 0;
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024;
    idx++;
  }
  if (idx === 0) {
    return value + ' ' + units[idx];
  }
  return value.toFixed(1) + ' ' + units[idx];
}

function humanRate(bytesPerSecond) {
  return `${humanBytes(bytesPerSecond)}/s`;
}

function renderConfigCards() {
  const list = document.getElementById('config-service-list');
  const previous = list._configSignature || '';
  const nextSignature = JSON.stringify(state.serviceViews.map(view => ({
    id: view?.config?.id || '',
    name: view?.config?.name || '',
    root: view?.root || '',
    path: view?.relative_path || '',
    args: view?.config?.args || [],
    auto_start: Boolean(view?.config?.auto_start),
    restart_on_exit: Boolean(view?.config?.restart_on_exit)
  })));
  if (previous === nextSignature) {
    return;
  }
  list._configSignature = nextSignature;
  list.innerHTML = '';
  const services = state.serviceViews.length ? state.serviceViews : [null];
  services.forEach(view => list.appendChild(createConfigCard(view)));
}

function createConfigCard(view) {
  const card = document.createElement('article');
  card.className = 'service-card config-card';
  const cfg = view ? view.config : {};
  const root = view?.root || '';
  const rel = view?.relative_path || '';
  card.innerHTML = `
    <div class="config-row">
      <label>服务 ID<input name="cfg-id" value="${escapeHtml(cfg.id || '')}" placeholder="example"/></label>
      <label>显示名称<input name="cfg-name" value="${escapeHtml(cfg.name || '')}" placeholder="可选"/></label>
    </div>
    <div class="config-row">
      <label>根目录<select name="cfg-root">${buildRootOptions(root)}</select></label>
      <label>执行路径<input name="cfg-path" value="${escapeHtml(rel)}" placeholder="bin/app"/></label>
    </div>
    <div class="config-row">
      <label>参数（每行一个）<textarea name="cfg-args">${escapeHtml((cfg.args || []).join('\n'))}</textarea></label>
    </div>
    <div class="config-row config-actions-inline">
      <label><input type="checkbox" name="cfg-autostart" ${cfg.auto_start ? 'checked' : ''}/> 自动启动</label>
      <label><input type="checkbox" name="cfg-restart" ${cfg.restart_on_exit ? 'checked' : ''}/> 退出重启</label>
      <button type="button" class="remove-config" data-action="remove-service">删除</button>
    </div>
  `;
  card.querySelector('[data-action="remove-service"]').addEventListener('click', () => {
    state.configDirty = true;
    card.remove();
  });
  card.querySelectorAll('input, textarea, select').forEach(input => {
    input.addEventListener('input', () => {
      state.configDirty = true;
    });
    input.addEventListener('change', () => {
      state.configDirty = true;
    });
  });
  return card;
}

function buildRootOptions(selected) {
  if (!state.roots.length) {
    return '<option value="">未包含根目录</option>';
  }
  return state.roots
    .map(root => `<option value="${escapeHtml(root)}" ${root === selected ? 'selected' : ''}>${escapeHtml(root)}</option>`)
    .join('');
}

function escapeHtml(text) {
  return String(text ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function parseArgs(text) {
  return text.split(/\r?\n/).map(line => line.trim()).filter(Boolean);
}

function collectServiceConfigs() {
  const cards = document.querySelectorAll('#config-service-list .config-card');
  const configs = [];
  const ids = new Set();
  for (const card of cards) {
    const id = card.querySelector('[name="cfg-id"]').value.trim();
    if (!id) {
      setNotice('请填写服务 ID', 'error');
      return null;
    }
    if (ids.has(id)) {
      setNotice(`服务 ID 重复：${id}`, 'error');
      return null;
    }
    ids.add(id);
    const root = card.querySelector('[name="cfg-root"]').value;
    if (!root) {
      setNotice('请选择根目录', 'error');
      return null;
    }
    const relPath = card.querySelector('[name="cfg-path"]').value.trim();
    if (!relPath) {
      setNotice('执行路径不能为空', 'error');
      return null;
    }
    configs.push({
      id,
      name: card.querySelector('[name="cfg-name"]').value.trim(),
      root,
      path: relPath,
      args: parseArgs(card.querySelector('[name="cfg-args"]').value),
      auto_start: card.querySelector('[name="cfg-autostart"]').checked,
      restart_on_exit: card.querySelector('[name="cfg-restart"]').checked
    });
  }
  return configs;
}

async function saveServiceConfig() {
  const configs = collectServiceConfigs();
  if (!configs) {
    return;
  }
  try {
    await fetchJSON('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ services: configs })
    });
    setNotice('服务配置已保存并应用', 'success');
    state.configDirty = false;
    document.getElementById('config-service-list')._configSignature = '';
    await refreshState();
  } catch (err) {
    setNotice('配置保存失败：' + err.message, 'error');
  }
}

function addConfigRow() {
  const list = document.getElementById('config-service-list');
  list.appendChild(createConfigCard());
}

function initFileTree() {
  ensureRootNodes();
  renderFileTree();
}

function ensureRootNodes() {
  const nodes = state.fileTree.nodes;
  const roots = [];
  (state.roots || []).forEach(root => {
    roots.push(root);
    if (!nodes.has(root)) {
      nodes.set(root, {
        path: root,
        name: basename(root) || root,
        isDir: true,
        expanded: false,
        loaded: false,
        children: []
      });
    }
  });
  state.fileTree.roots = roots;
}

async function fetchDirectory(path) {
  const query = path ? `?path=${encodeURIComponent(path)}` : '';
  const list = await fetchJSON(`/api/files${query}`);
  return (list || []).filter(entry => !entry.name.startsWith('.'));
}

async function toggleNode(path) {
  const node = state.fileTree.nodes.get(path);
  if (!node || !node.isDir) {
    return;
  }
  node.expanded = !node.expanded;
  if (node.expanded && !node.loaded) {
    node.loading = true;
    renderFileTree();
    try {
      const entries = await fetchDirectory(node.path);
      node.children = entries.map(entry => {
        if (!state.fileTree.nodes.has(entry.path)) {
          state.fileTree.nodes.set(entry.path, {
            path: entry.path,
            name: entry.name,
            isDir: entry.is_dir,
            expanded: false,
            loaded: false,
            children: []
          });
        }
        return entry.path;
      });
      node.loaded = true;
    } catch (err) {
      setNotice('目录读取失败：' + err.message, 'error');
    } finally {
      node.loading = false;
    }
  }
  renderFileTree();
}

function confirmDiscardUnsavedFile() {
  if (!state.fileDirty) {
    return true;
  }
  const confirmed = window.confirm('当前文件有未保存修改，继续操作会丢失这些更改。是否继续？');
  if (confirmed) {
    state.fileDirty = false;
  }
  return confirmed;
}

function renderFileTree() {
  const container = document.getElementById('file-tree');
  container.innerHTML = '';
  if (!state.fileTree.roots.length) {
    container.innerHTML = '<p class="muted small">暂无可用目录</p>';
    return;
  }
  state.fileTree.roots.forEach(root => {
    const nodeEl = buildTreeNode(root);
    if (nodeEl) {
      container.appendChild(nodeEl);
    }
  });
}

function buildTreeNode(path) {
  const node = state.fileTree.nodes.get(path);
  if (!node) {
    return null;
  }
  const wrapper = document.createElement('div');
  const row = document.createElement('div');
  row.className = `tree-node ${node.isDir ? 'is-dir' : 'is-file'}`;
  row.dataset.expanded = node.expanded ? 'true' : 'false';
  row.innerHTML = `
    <span class="tree-toggle">▸</span>
    <span class="tree-label">${escapeHtml(node.name)}</span>
  `;
  row.addEventListener('click', () => {
    if (node.isDir) {
      state.currentPath = node.path;
      document.getElementById('file-path').textContent = state.currentPath || '根目录';
      toggleNode(node.path);
    } else {
      loadFile(node.path);
    }
  });
  wrapper.appendChild(row);

  if (node.isDir) {
    const children = document.createElement('div');
    children.className = `tree-children ${node.expanded ? 'open' : ''}`;
    if (node.loading) {
      children.innerHTML = '<p class="muted small">加载中…</p>';
    } else if (node.loaded && node.children.length === 0) {
      children.innerHTML = '<p class="muted small">空目录</p>';
    } else if (node.expanded) {
      node.children.forEach(childPath => {
        const childEl = buildTreeNode(childPath);
        if (childEl) {
          children.appendChild(childEl);
        }
      });
    }
    wrapper.appendChild(children);
  }
  return wrapper;
}

async function loadFile(path) {
  if (state.selectedFile !== path && !confirmDiscardUnsavedFile()) {
    return;
  }
  try {
  const payload = await fetchJSON(`/api/files/content?path=${encodeURIComponent(path)}`);
    const textarea = document.getElementById('file-content');
    textarea.value = payload.content || '';
    state.selectedFile = path;
    state.selectedEncoding = payload.encoding || 'utf-8';
    document.getElementById('file-encoding').textContent = state.selectedEncoding;
    document.getElementById('file-save').disabled = false;
    state.fileDirty = false;
    state.currentPath = path;
    document.getElementById('file-path').textContent = path || '根目录';
    updateFileMeta(path);
    updateLineNumbers();
  } catch (err) {
    setNotice('读取文件失败：' + err.message, 'error');
  }
}

function updateFileMeta(path) {
  const nameEl = document.getElementById('file-name');
  const typeEl = document.getElementById('file-type');
  const name = path ? basename(path) : '未选择';
  nameEl.textContent = name || '未选择';
  const type = detectLanguage(path || '');
  typeEl.textContent = type.label;
  typeEl.classList.toggle('badge-muted', type.tone === 'muted');
  typeEl.classList.toggle('badge-success', type.tone === 'accent');
}

function detectLanguage(path) {
  const ext = path.includes('.') ? path.toLowerCase().split('.').pop() : '';
  const map = {
    json: 'JSON',
    yaml: 'YAML',
    yml: 'YAML',
    toml: 'TOML',
    ini: 'INI',
    conf: 'CONF',
    cfg: 'CFG',
    env: 'ENV',
    properties: 'PROPS',
    xml: 'XML',
    txt: 'TEXT',
    md: 'MARKDOWN'
  };
  const label = map[ext] || 'TEXT';
  return { label, tone: label === 'TEXT' ? 'muted' : 'accent' };
}

function basename(path) {
  if (!path) return '';
  const parts = path.split('/');
  return parts[parts.length - 1];
}

function updateLineNumbers() {
  const textarea = document.getElementById('file-content');
  const lineNumbers = document.getElementById('line-numbers');
  if (!textarea || !lineNumbers) return;
  const lines = textarea.value.split('\n').length || 1;
  let html = '';
  for (let i = 1; i <= lines; i++) {
    html += `<span>${i}</span>`;
  }
  lineNumbers.innerHTML = html;
  syncLineNumbers();
}

function syncLineNumbers() {
  const textarea = document.getElementById('file-content');
  const lineNumbers = document.getElementById('line-numbers');
  if (!textarea || !lineNumbers) return;
  lineNumbers.scrollTop = textarea.scrollTop;
}

function toggleWrap() {
  state.wrapEnabled = !state.wrapEnabled;
  const textarea = document.getElementById('file-content');
  const button = document.getElementById('toggle-wrap');
  textarea.wrap = state.wrapEnabled ? 'soft' : 'off';
  button.classList.toggle('active', state.wrapEnabled);
}

function insertTextAtCursor(text) {
  const textarea = document.getElementById('file-content');
  const start = textarea.selectionStart;
  const end = textarea.selectionEnd;
  textarea.setRangeText(text, start, end, 'end');
  updateLineNumbers();
}

function handleEditorKeydown(event) {
  const textarea = document.getElementById('file-content');
  if (event.key === 'Tab') {
    event.preventDefault();
    insertTextAtCursor('  ');
    state.fileDirty = true;
  } else if (event.key === 'Enter') {
    const start = textarea.selectionStart;
    const before = textarea.value.slice(0, start);
    const line = before.split('\n').pop() || '';
    const indent = line.match(/^\s*/)?.[0] || '';
    event.preventDefault();
    insertTextAtCursor(`\n${indent}`);
    state.fileDirty = true;
  } else if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
    event.preventDefault();
    saveFile();
  }
}

async function saveFile() {
  if (!state.selectedFile) {
    setNotice('请选择一个文件再保存', 'error');
    return;
  }
  const content = document.getElementById('file-content').value;
  try {
    await fetchJSON(`/api/files/content?path=${encodeURIComponent(state.selectedFile)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content, encoding: 'utf-8' })
    });
    setNotice('文件保存成功', 'success');
    state.fileDirty = false;
  } catch (err) {
    setNotice('保存失败：' + err.message, 'error');
  }
}

async function saveHTTPSConfig() {
  const memoryEl = document.getElementById('mocker-memory-limit');
  const memoryLimitMB = Number((memoryEl.value || '').trim());
  if (!memoryLimitMB || Number.isNaN(memoryLimitMB) || memoryLimitMB < 16 || memoryLimitMB > 1048576) {
    setNotice('内存上限需为 16-1048576 MB', 'error');
    return;
  }
  const certFile = document.getElementById('http-cert-file').value.trim();
  const keyFile = document.getElementById('http-key-file').value.trim();
  if (!certFile || !keyFile) {
    setNotice('HTTPS 证书和私钥路径不能为空', 'error');
    return;
  }
  const address = state.httpAddress || '0.0.0.0:8090';
  try {
    const payload = await fetchJSON('/api/system/http', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address, cert_file: certFile, key_file: keyFile, memory_limit_mb: memoryLimitMB })
    });
    state.httpAddress = payload.address || address;
    state.certFile = payload.cert_file || certFile;
    state.keyFile = payload.key_file || keyFile;
    state.memoryLimitMB = payload.memory_limit_mb || memoryLimitMB;
    state.systemDirty = false;
    state.isEditingSystem = false;
    setNotice('HTTPS 配置已保存，需重启进程后生效', 'success');
  } catch (err) {
    setNotice('保存失败：' + err.message, 'error');
  }
}

function activateTab(tabId) {
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.tab === tabId);
  });
  document.querySelectorAll('.tab-panel').forEach(panel => {
    panel.classList.toggle('active', panel.dataset.tab === tabId);
  });
}

async function initAuth() {
  try {
    const me = await fetchJSON('/api/auth/me');
    if (me && me.enabled === false) {
      state.loggedIn = true;
      state.user = '';
      showApp();
      return;
    }
    state.loggedIn = true;
    state.user = me.username || '';
    showApp();
  } catch (err) {
    showLogin('');
  }
}

async function submitLogin(event) {
  event.preventDefault();
  const username = document.getElementById('login-username').value.trim();
  const password = document.getElementById('login-password').value;
  const error = document.getElementById('login-error');
  error.textContent = '';
  try {
    const response = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    if (!response.ok) {
      error.textContent = '账号或密码错误';
      return;
    }
    const payload = await response.json();
    state.loggedIn = true;
    state.user = payload.username || username;
    showApp();
  } catch (err) {
    error.textContent = '登录失败：' + err.message;
  }
}

async function submitPassword(event) {
  event.preventDefault();
  const current = document.getElementById('current-password').value;
  const next = document.getElementById('new-password').value;
  const confirm = document.getElementById('confirm-password').value;
  if (!next || next.length < 6) {
    setNotice('新密码至少 6 位', 'error');
    return;
  }
  if (next !== confirm) {
    setNotice('两次密码不一致', 'error');
    return;
  }
  try {
    await fetchJSON('/api/auth/password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ current_password: current, new_password: next })
    });
    setNotice('密码已更新', 'success');
    document.getElementById('current-password').value = '';
    document.getElementById('new-password').value = '';
    document.getElementById('confirm-password').value = '';
  } catch (err) {
    setNotice('修改失败：' + err.message, 'error');
  }
}

async function logout() {
  try {
    await fetchJSON('/api/auth/logout', { method: 'POST' });
  } catch (err) {
    // ignore
  }
  state.loggedIn = false;
  state.user = '';
  showLogin('');
}

document.addEventListener('DOMContentLoaded', () => {
  initAuth();
  document.getElementById('file-save').addEventListener('click', saveFile);
  document.getElementById('add-config-row').addEventListener('click', addConfigRow);
  document.getElementById('save-config').addEventListener('click', saveServiceConfig);
  document.getElementById('login-form').addEventListener('submit', submitLogin);
  document.getElementById('password-form').addEventListener('submit', submitPassword);
  document.getElementById('logout-btn').addEventListener('click', logout);
  document.getElementById('save-http').addEventListener('click', saveHTTPSConfig);
  document.getElementById('toggle-wrap').addEventListener('click', toggleWrap);
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => activateTab(btn.dataset.tab));
  });
  document.getElementById('file-content').addEventListener('input', () => {
    state.fileDirty = true;
    updateLineNumbers();
  });
  document.getElementById('file-content').addEventListener('scroll', syncLineNumbers);
  document.getElementById('file-content').addEventListener('keydown', handleEditorKeydown);
  document.getElementById('file-content').wrap = 'off';
  window.addEventListener('beforeunload', event => {
    if (!state.fileDirty && !state.configDirty && !state.systemDirty) {
      return;
    }
    event.preventDefault();
    event.returnValue = '';
  });
  document.addEventListener('focusin', event => {
    if (event.target.closest('.config-panel')) {
      state.isEditingConfig = true;
    }
    if (event.target.closest('.file-editor')) {
      state.isEditingFile = true;
    }
    if (event.target.closest('.account-panel')) {
      state.isEditingSystem = true;
    }
  });
  document.addEventListener('focusout', event => {
    if (event.target.closest('.config-panel')) {
      setTimeout(() => {
        state.isEditingConfig = !!document.activeElement?.closest('.config-panel');
      }, 0);
    }
    if (event.target.closest('.file-editor')) {
      setTimeout(() => {
        state.isEditingFile = !!document.activeElement?.closest('.file-editor');
      }, 0);
    }
    if (event.target.closest('.account-panel')) {
      setTimeout(() => {
        state.isEditingSystem = !!document.activeElement?.closest('.account-panel');
      }, 0);
    }
  });
  const memoryEl = document.getElementById('mocker-memory-limit');
  if (memoryEl) {
    memoryEl.addEventListener('input', () => {
      state.systemDirty = true;
    });
  }
  const certEl = document.getElementById('http-cert-file');
  if (certEl) {
    certEl.addEventListener('input', () => {
      state.systemDirty = true;
    });
  }
  const keyEl = document.getElementById('http-key-file');
  if (keyEl) {
    keyEl.addEventListener('input', () => {
      state.systemDirty = true;
    });
  }
  updateFileMeta('');
  updateLineNumbers();
});
