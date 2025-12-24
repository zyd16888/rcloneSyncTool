function createLineChart(canvas, opts) {
  const options = opts || {};
  const padding = options.padding || 36;
  const color = options.color || "#16baaa";
  const grid = options.grid || "rgba(0,0,0,0.08)";
  const text = options.text || "rgba(0,0,0,0.55)";
  const bg = options.bg || "#ffffff";

  const ctx = canvas.getContext("2d");

  function draw(points, formatter) {
    const w = canvas.width;
    const h = canvas.height;
    ctx.clearRect(0, 0, w, h);

    ctx.fillStyle = bg;
    ctx.fillRect(0, 0, w, h);

    if (!points || points.length < 2) {
      ctx.fillStyle = text;
      ctx.font = "12px sans-serif";
      ctx.fillText("暂无数据", padding, padding);
      return;
    }

    let minY = Infinity, maxY = -Infinity;
    for (const p of points) {
      if (p.y < minY) minY = p.y;
      if (p.y > maxY) maxY = p.y;
    }
    if (!isFinite(minY) || !isFinite(maxY)) return;
    if (minY === maxY) { maxY = minY + 1; }

    const minX = points[0].x;
    const maxX = points[points.length - 1].x;
    const plotW = w - padding * 2;
    const plotH = h - padding * 2;

    const xToPx = (x) => padding + (x - minX) * plotW / Math.max(1, (maxX - minX));
    const yToPx = (y) => padding + (maxY - y) * plotH / Math.max(1e-9, (maxY - minY));

    // grid
    ctx.strokeStyle = grid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    for (let i = 0; i <= 4; i++) {
      const y = padding + i * (plotH / 4);
      ctx.moveTo(padding, y);
      ctx.lineTo(w - padding, y);
    }
    ctx.stroke();

    // axes labels (min/max)
    ctx.fillStyle = text;
    ctx.font = "12px sans-serif";
    const fmt = formatter || ((v) => String(v));
    ctx.fillText(fmt(maxY), 6, padding + 4);
    ctx.fillText(fmt(minY), 6, h - padding + 4);

    // line
    ctx.strokeStyle = color;
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.moveTo(xToPx(points[0].x), yToPx(points[0].y));
    for (let i = 1; i < points.length; i++) {
      ctx.lineTo(xToPx(points[i].x), yToPx(points[i].y));
    }
    ctx.stroke();

    // last point marker
    const last = points[points.length - 1];
    ctx.fillStyle = color;
    ctx.beginPath();
    ctx.arc(xToPx(last.x), yToPx(last.y), 3, 0, Math.PI * 2);
    ctx.fill();
  }

  return { draw };
}

