const GLOSSARY: [string, string][] = [
  ['API server', 'The front door of the control plane. Authenticates and authorizes every request, then stores the desired state (in etcd). KubeScope only ever talks to it.'],
  ['Desired vs actual state', 'You declare what you want (spec.replicas = 4); controllers keep working until reality (status) matches.'],
  ['Deployment controller', 'Turns a Deployment spec into ReplicaSets and resizes them.'],
  ['ReplicaSet', 'Keeps N identical Pods alive. Delete one and it creates a replacement with a brand-new UID.'],
  ['Scheduler', 'Picks a node for each new Pod (sets spec.nodeName).'],
  ['Kubelet', 'Agent on each node that starts containers and runs readiness probes.'],
  ['Running vs Ready', 'Running: the container process started. Ready: the readiness probe passes, so the Pod receives traffic.'],
  ['generation / observedGeneration', 'generation increments when the spec changes; observedGeneration shows which spec the controller has processed. Status is only trustworthy once they match.'],
  ['Watch & resourceVersion', 'Clients stream changes from a resourceVersion. Streams can drop or expire (410 Gone); clients then re-list and resume.'],
  ['GKE', 'Google runs the control plane for you. KubeScope uses the same public Kubernetes API there — it has no view inside Google’s infrastructure.'],
];

export function LearnPanel() {
  return (
    <section className="panel" aria-labelledby="learn-h">
      <h2 id="learn-h">Learn</h2>
      <dl className="glossary">
        {GLOSSARY.map(([term, def]) => (
          <div key={term}>
            <dt>{term}</dt>
            <dd>{def}</dd>
          </div>
        ))}
      </dl>
      <p>
        Read more: <a href="/docs/LEARNING_GUIDE.md" target="_blank" rel="noreferrer">Learning guide</a> ·{' '}
        <a href="/docs/GKE_SETUP_TUTORIAL.html" target="_blank" rel="noreferrer">GKE setup tutorial</a>
      </p>
    </section>
  );
}
