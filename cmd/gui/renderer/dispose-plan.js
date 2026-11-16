// 释放 three.js 子树的**决策**（纯逻辑，不调用 dispose、不碰 THREE）。
//
// ## 为什么单独一个模块
//
// `disposeStarmapObject` 里的核心是一条**遍历 + 去重 + 排除共享**的规则：
//
//	· 同一 geometry / material / texture **只释放一次**（Set 去重）
//	· 共享资源（starmapUnitSphere）**永不释放** —— dispose 掉它会让仍在
//	  渲染的 mesh 变黑/报错
//	· 纹理挂在 material 的任意字段上（map / 多通道贴图），要逐个查 isTexture
//
// 判据只能正则匹配源码里有没有 `g !== starmapUnitSphere` 之类 —— 读源码断言。
//
// ## 接口
//
//	planDispose(root, shared) → { geometries: [...], materials: [...], textures: [...] }
//	  root   : three.js 对象树（只需有 .traverse 与 .geometry/.material 字段）
//	  shared : 共享资源集合（永不释放）
//
// 返回**待释放清单**，调用方（app.js）照着执行 dispose()。
// 这样决策可单测，而副作用仍留在原处。
//
// ★ 判据能回答的问题：给一棵带重复几何/共享几何/多通道纹理的假树，
//   清单里是否①去掉重复②排除共享③收齐纹理。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.DisposePlan = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  function planDispose(root, shared) {
    var geo = [],
      mat = [],
      tex = [];
    var seenGeo = new Set(),
      seenMat = new Set(),
      seenTex = new Set();

    function addTex(mm) {
      if (!mm || typeof mm !== "object") return;
      Object.keys(mm).forEach(function (k) {
        var v = mm[k];
        if (v && v.isTexture && !seenTex.has(v) && v !== shared) {
          seenTex.add(v);
          tex.push(v);
        }
      });
    }

    function walk(o) {
      if (!o) return;
      // geometry
      var g = o.geometry;
      if (g && g !== shared && !seenGeo.has(g)) {
        seenGeo.add(g);
        geo.push(g);
      }
      // material（可能是数组）
      var m = o.material;
      if (m) {
        var mats = Array.isArray(m) ? m : [m];
        mats.forEach(function (mm) {
          if (!mm || seenMat.has(mm) || mm === shared) return;
          seenMat.add(mm);
          addTex(mm);
          mat.push(mm);
        });
      }
    }

    // root 自身 + 全部子孙（three.js 的 traverse 含自身，这里等价实现）
    if (typeof root.traverse === "function") root.traverse(walk);
    else walk(root);

    return { geometries: geo, materials: mat, textures: tex };
  }

  return { planDispose: planDispose };
});
