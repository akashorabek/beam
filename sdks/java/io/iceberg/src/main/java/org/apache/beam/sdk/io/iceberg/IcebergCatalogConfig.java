/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package org.apache.beam.sdk.io.iceberg;

import com.google.auto.value.AutoValue;
import java.io.Serializable;
import java.util.Map;
import org.apache.beam.sdk.util.ReleaseInfo;
import org.apache.beam.vendor.guava.v32_1_2_jre.com.google.common.collect.Maps;
import org.apache.hadoop.conf.Configuration;
import org.apache.iceberg.CatalogUtil;
import org.apache.iceberg.catalog.Catalog;
import org.checkerframework.checker.nullness.qual.MonotonicNonNull;
import org.checkerframework.checker.nullness.qual.Nullable;
import org.checkerframework.dataflow.qual.Pure;

@AutoValue
public abstract class IcebergCatalogConfig implements Serializable {
  private transient @MonotonicNonNull Catalog cachedCatalog;

  @Pure
  @Nullable
  public abstract String getCatalogName();

  @Pure
  @Nullable
  public abstract Map<String, String> getCatalogProperties();

  @Pure
  @Nullable
  public abstract Map<String, String> getConfigProperties();

  @Pure
  public static Builder builder() {
    return new AutoValue_IcebergCatalogConfig.Builder();
  }

  public abstract Builder toBuilder();

  public org.apache.iceberg.catalog.Catalog catalog() {
    if (cachedCatalog == null) {
      String catalogName = getCatalogName();
      if (catalogName == null) {
        catalogName = "apache-beam-" + ReleaseInfo.getReleaseInfo().getVersion();
      }
      Map<String, String> catalogProps = getCatalogProperties();
      if (catalogProps == null) {
        catalogProps = Maps.newHashMap();
      }
      Map<String, String> confProps = getConfigProperties();
      if (confProps == null) {
        confProps = Maps.newHashMap();
      }
      Configuration config = new Configuration();
      for (Map.Entry<String, String> prop : confProps.entrySet()) {
        config.set(prop.getKey(), prop.getValue());
      }
      cachedCatalog = CatalogUtil.buildIcebergCatalog(catalogName, catalogProps, config);
    }
    return cachedCatalog;
  }

  @AutoValue.Builder
  public abstract static class Builder {
    public abstract Builder setCatalogName(@Nullable String catalogName);

    public abstract Builder setCatalogProperties(@Nullable Map<String, String> props);

    public abstract Builder setConfigProperties(@Nullable Map<String, String> props);

    public abstract IcebergCatalogConfig build();
  }
}
