<script lang="ts">
  import type { infrastructureSelection } from "../lib/models";
  export let data: infrastructureSelection;
</script>

<input type="hidden" name="view" value="infrastructure" />
<input type="hidden" name="node" value={data.Node} />
<input type="hidden" name="organization" value={data.Organization} />
<input type="hidden" name="cluster" value={data.Cluster} />
<input type="hidden" name="tab" value={data.Tab} />
<input type="hidden" name="componentScope" value={data.ComponentScope} />
<input type="hidden" name="component" value={data.Component} />
<input type="hidden" name="managementOperation" value={data.Operation} />
